package raftnode

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/raft/rafttest"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
)

func TestTransportCloseDrainsHeartbeat(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	brokers := watch.NewRegistry()
	n, err := New(Config{
		DataDir: dir, LocalID: "node-a", BindAddr: "127.0.0.1:0",
		FSM: fsm.New(state.New(brokers), brokers), LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	n.transport.SetHeartbeatHandler(func(rpc hraft.RPC) {
		close(entered)
		<-release
		rpc.Respond(&hraft.AppendEntriesResponse{Success: true}, nil)
	})
	cfg, err := loadNodeTLS(dir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerName = "node-a"
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", string(n.LocalAddr()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if err := codec.NewEncoder(conn, &codec.MsgpackHandle{}).Encode(&hraft.AppendEntriesRequest{
		RPCHeader: hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte("node-a"), Addr: []byte(n.LocalAddr())},
		Term:      1,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat did not enter the handler")
	}
	closer, ok := n.transport.(interface{ Close() error })
	if !ok {
		t.Fatal("transport cannot close")
	}
	closed := make(chan error, 1)
	go func() { closed <- closer.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("transport closed while heartbeat still used Raft state: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("transport did not close after heartbeat finished")
	}
}

func TestRaftDialPinsServerIDNotJustAnyClusterCertificate(t *testing.T) {
	caCert, caKey := rafttest.NewCA(t)
	nodes := make(map[string]*Node)
	dirs := make(map[string]string)
	for _, id := range []string{"node-a", "node-c"} {
		dir := t.TempDir()
		dirs[id] = dir
		rafttest.Issue(t, dir, id, caCert, caKey)
		brokers := watch.NewRegistry()
		n, err := New(Config{
			DataDir: dir, LocalID: id, BindAddr: "127.0.0.1:0", Bootstrap: id == "node-a",
			FSM: fsm.New(state.New(brokers), brokers), LogOutput: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		nodes[id] = n
		t.Cleanup(func() { _ = n.Shutdown() })
	}
	a, c := nodes["node-a"], nodes["node-c"]
	deadline := time.Now().Add(10 * time.Second)
	for !a.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.Raft.AddNonvoter("node-b", c.LocalAddr(), 0, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	vote := &hraft.RequestVoteRequest{
		RPCHeader: hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte("node-a"), Addr: []byte(a.LocalAddr())},
		Term:      1,
	}
	if err := a.transport.RequestVote("node-b", c.LocalAddr(), vote, &hraft.RequestVoteResponse{}); err == nil {
		t.Fatal("Raft dial accepted node-c's certificate for node-b")
	}

	// Even a certificate with a broad SAN cannot substitute a different
	// node's logical identity for the destination Raft server ID.
	cfg, err := loadNodeTLS(dirs["node-c"], "node-c")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	authority, key, err := ca.ParseCA(caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf.DNSNames = append(leaf.DNSNames, "node-b")
	der, err := x509.CreateCertificate(rand.Reader, leaf, authority, leaf.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs["node-c"], "node", "node-c.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	err = a.transport.RequestVote("node-b", c.LocalAddr(), vote, &hraft.RequestVoteResponse{})
	if err == nil || !strings.Contains(err.Error(), `unexpected peer identity "node-c"`) {
		t.Fatalf("broad-SAN identity substitution: %v", err)
	}
}

func TestRaftPooledConnectionCannotFollowReassignedAddress(t *testing.T) {
	caCert, caKey := rafttest.NewCA(t)
	dir := t.TempDir()
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	brokers := watch.NewRegistry()
	a, err := New(Config{
		DataDir: dir, LocalID: "node-a", BindAddr: "127.0.0.1:0", Bootstrap: true,
		FSM: fsm.New(state.New(brokers), brokers), LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	cDir := t.TempDir()
	rafttest.Issue(t, cDir, "node-c", caCert, caKey)
	cBrokers := watch.NewRegistry()
	c, err := New(Config{
		DataDir: cDir, LocalID: "node-c", BindAddr: "127.0.0.1:0",
		FSM: fsm.New(state.New(cBrokers), cBrokers), LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown() })
	deadline := time.Now().Add(10 * time.Second)
	for !a.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.Raft.AddNonvoter("node-c", c.LocalAddr(), 0, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	// Hold an authenticated connection, as the network transport does when
	// caching by address rather than by logical server ID.
	stream := &tlsStream{dataDir: dir, localID: "node-a", conns: make(map[*tlsRaftConn]struct{})}
	stream.raft.Store(a.Raft)
	conn, err := stream.Dial(c.LocalAddr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := a.Raft.RemoveServer("node-c", 0, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	if err := a.Raft.AddNonvoter("replacement", c.LocalAddr(), 0, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{1}); err == nil {
		t.Fatal("authenticated connection remained usable after its Raft address changed identity")
	}
}

func TestRaftPooledConnectionRejectsExpiredIssuer(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{
		Subject: pkix.Name{CommonName: "node-a"}, DNSNames: []string{"node-a"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	}
	issuer := &x509.Certificate{NotBefore: now.Add(-time.Hour), NotAfter: now.Add(-time.Minute)}
	cs := tls.ConnectionState{
		NegotiatedProtocol: raftALPN, PeerCertificates: []*x509.Certificate{leaf},
		VerifiedChains: [][]*x509.Certificate{{leaf, issuer}},
	}
	if err := (&tlsStream{}).authorize(cs, "node-a"); err == nil {
		t.Fatal("previously verified connection remained authenticated after its CA expired")
	}
	validIssuer := &x509.Certificate{NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	cs.VerifiedChains = append(cs.VerifiedChains, []*x509.Certificate{leaf, validIssuer})
	if err := (&tlsStream{}).authorize(cs, "node-a"); err != nil {
		t.Fatalf("valid alternate verified chain rejected: %v", err)
	}
}
