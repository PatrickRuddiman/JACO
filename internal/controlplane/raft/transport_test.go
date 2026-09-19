package raftnode_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/raft/rafttest"
)

func TestRaftRejectsPlaintext(t *testing.T) {
	dir := t.TempDir()
	cert, key := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", cert, key)
	n, err := raftnode.New(raftnode.Config{
		DataDir: dir, BindAddr: "127.0.0.1:0", LocalID: "node-a",
		Bootstrap: true, FSM: noopFSM{}, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	plain, err := hraft.NewTCPTransport("127.0.0.1:0", nil, 1, 10*time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	err = plain.RequestVote("node-a", n.LocalAddr(), &hraft.RequestVoteRequest{
		RPCHeader: hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte("outsider"), Addr: []byte(plain.LocalAddr())},
		Term:      1,
	}, &hraft.RequestVoteResponse{})
	if err == nil {
		t.Fatal("Raft accepted an unauthenticated plaintext RPC")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("timeout is not proof of plaintext rejection: %v", err)
	}
}

func TestRaftTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	cert, key := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", cert, key)
	n, err := raftnode.New(raftnode.Config{
		DataDir: dir, BindAddr: "127.0.0.1:0", LocalID: "node-a",
		Bootstrap: true, FSM: noopFSM{}, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	clientCert, err := tls.LoadX509KeyPair(filepath.Join(dir, "node", "node-a.crt"), filepath.Join(dir, "node", "node-a.key"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", string(n.LocalAddr()), &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "node-a",
		Certificates: []tls.Certificate{clientCert}, NextProtos: []string{"jaco-raft-v1"},
	})
	if err != nil {
		t.Fatalf("authenticated Raft TLS handshake: %v", err)
	}
	defer conn.Close()
	if cs := conn.ConnectionState(); cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != "jaco-raft-v1" {
		t.Fatalf("unexpected Raft TLS negotiation: version=%x protocol=%q", cs.Version, cs.NegotiatedProtocol)
	}
}

func TestRaftRejectsUntrustedCredentials(t *testing.T) {
	caCert, caKey := rafttest.NewCA(t)
	dir := t.TempDir()
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	n := transportNode(t, dir, "node-a", "127.0.0.1:0", "", true, noopFSM{})
	good := clientTLS(t, dir, "node-a", caCert, "node-a")
	wrongCert, wrongKey := rafttest.NewCA(t)
	wrongDir := t.TempDir()
	rafttest.Issue(t, wrongDir, "node-a", wrongCert, wrongKey)
	outsiderDir := t.TempDir()
	rafttest.Issue(t, outsiderDir, "outsider", caCert, caKey)
	noALPN := good.Clone()
	noALPN.NextProtos = nil
	tls12 := good.Clone()
	tls12.MinVersion, tls12.MaxVersion = tls.VersionTLS12, tls.VersionTLS12

	cases := []struct {
		name string
		cfg  *tls.Config
	}{
		{"TLS 1.2", tls12},
		{"missing Raft ALPN", noALPN},
		{"no certificate", &tls.Config{RootCAs: good.RootCAs, ServerName: "node-a", NextProtos: good.NextProtos}},
		{"wrong client CA", clientTLS(t, wrongDir, "node-a", caCert, "node-a")},
		{"wrong server CA", clientTLS(t, dir, "node-a", wrongCert, "node-a")},
		{"wrong server identity", clientTLS(t, dir, "node-a", caCert, "node-b")},
		{"expired client", alteredClientTLS(t, dir, "node-a", caCert, caKey, func(cert *x509.Certificate) {
			cert.NotAfter = time.Now().Add(-time.Minute)
		})},
		{"server-only certificate", alteredClientTLS(t, dir, "node-a", caCert, caKey, func(cert *x509.Certificate) {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		})},
		{"CA used as a node", alteredClientTLS(t, dir, "node-a", caCert, caKey, func(cert *x509.Certificate) {
			cert.IsCA, cert.BasicConstraintsValid = true, true
		})},
		{"missing identity SAN", alteredClientTLS(t, dir, "node-a", caCert, caKey, func(cert *x509.Certificate) {
			cert.DNSNames = nil
		})},
		{"missing identity CN", alteredClientTLS(t, dir, "node-a", caCert, caKey, func(cert *x509.Certificate) {
			cert.Subject.CommonName = ""
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", string(n.LocalAddr()), tc.cfg)
			if err == nil {
				defer conn.Close()
				err = requestVote(conn, "node-a")
			}
			if err == nil {
				t.Fatal("untrusted peer reached the Raft protocol")
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("timeout is not proof of credential rejection: %v", err)
			}
		})
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", string(n.LocalAddr()), good)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := requestVote(conn, "node-a"); err != nil {
		t.Fatalf("authenticated control request failed: %v", err)
	}
	newPeer, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", string(n.LocalAddr()),
		clientTLS(t, outsiderDir, "outsider", caCert, "node-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer newPeer.Close()
	if err := requestVote(newPeer, "outsider"); err != nil {
		t.Fatalf("CA-issued node identity rejected before local membership catches up: %v", err)
	}
}

func TestRaftExpiredConnectionCannotDispatchRPC(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	n := transportNode(t, dir, "node-a", "127.0.0.1:0", "", false, noopFSM{})
	observations := make(chan hraft.Observation, 2)
	observer := hraft.NewObserver(observations, false, func(o *hraft.Observation) bool {
		_, ok := o.Data.(hraft.RequestVoteRequest)
		return ok
	})
	n.Raft.RegisterObserver(observer)
	defer n.Raft.DeregisterObserver(observer)
	expires := time.Now().Add(3 * time.Second).Truncate(time.Second)
	cfg := alteredClientTLS(t, dir, "node-a", caCert, caKey, func(cert *x509.Certificate) {
		cert.NotAfter = expires
	})
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", string(n.LocalAddr()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := requestVote(conn, "node-a"); err != nil {
		t.Fatalf("valid pre-expiry RPC: %v", err)
	}
	select {
	case <-observations:
	default:
		t.Fatal("control RPC was not observed")
	}
	time.Sleep(time.Until(expires.Add(50 * time.Millisecond)))
	var message bytes.Buffer
	message.WriteByte(1)
	if err := codec.NewEncoder(&message, &codec.MsgpackHandle{}).Encode(&hraft.RequestVoteRequest{
		RPCHeader: hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte("node-a"), Addr: []byte(n.LocalAddr())},
		Term:      1,
	}); err != nil {
		t.Fatal(err)
	}
	// One TLS record exercises an already-blocked read, not just the next
	// reader call's preflight check.
	if _, err := conn.Write(message.Bytes()); err == nil {
		_, _ = conn.Read(make([]byte, 1))
	}
	select {
	case <-observations:
		t.Fatal("Raft dispatched an RPC received after its authenticated certificate expired")
	default:
	}
}

func TestRaftCredentialReloadFailsClosed(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	n := transportNode(t, dir, "node-a", "127.0.0.1:0", "", false, noopFSM{})
	cfg := clientTLS(t, dir, "node-a", caCert, "node-a")
	probe := func() error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", string(n.LocalAddr()), cfg)
		if err != nil {
			return err
		}
		defer conn.Close()
		return requestVote(conn, "node-a")
	}
	if err := probe(); err != nil {
		t.Fatalf("initial authenticated connection: %v", err)
	}
	for _, name := range []string{"node-a.crt", "node-a.key", "ca.crt"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "node", name)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("invalid replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := probe(); err == nil {
				t.Fatal("invalid credentials fell back to the previous identity")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("timeout is not proof of failed-closed credential reload: %v", err)
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := probe(); err != nil {
				t.Fatalf("valid credential repair was not reloaded: %v", err)
			}
		})
	}
}

func TestRaftTLSReplicationAndSnapshotCatchup(t *testing.T) {
	caCert, caKey := rafttest.NewCA(t)
	aDir, bDir := t.TempDir(), t.TempDir()
	rafttest.Issue(t, aDir, "node-a", caCert, caKey)
	rafttest.Issue(t, bDir, "node-b", caCert, caKey)
	aFSM := &recordFSM{}
	a := transportNode(t, aDir, "node-a", "127.0.0.1:0", "", true, aFSM)
	if !waitForLeader(t, a, 10*time.Second) {
		t.Fatal("bootstrap did not elect leader")
	}
	bAddr := unusedAddress(t)
	proxy := newWireProxy(t, bAddr)
	bFSM := &recordFSM{}
	b := transportNode(t, bDir, "node-b", bAddr, proxy.Addr(), false, bFSM)
	if err := a.Raft.AddNonvoter("node-b", b.LocalAddr(), 0, 10*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	const secret = "RAFT_WIRE_CANARY_private_key_registry_password_compose_secret"
	if _, err := a.Apply([]byte(secret), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	awaitTransport(t, "encrypted AppendEntries replication", func() bool { return bFSM.contains(secret) })
	if wire := proxy.captured(); len(wire) == 0 || bytes.Contains(wire, []byte(secret)) {
		t.Fatalf("AppendEntries secret was not protected on the wire (%d captured bytes)", len(wire))
	}

	proxy.disconnect()
	const reconnect = "reconnected-after-pooled-stream-closure"
	if _, err := a.Apply([]byte(reconnect), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	awaitTransport(t, "reconnection", func() bool { return bFSM.contains(reconnect) })

	if err := b.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// Rotate both leaf keypairs under the same CA; reconnects must reload them.
	rafttest.Issue(t, aDir, "node-a", caCert, caKey)
	rafttest.Issue(t, bDir, "node-b", caCert, caKey)
	rc := a.Raft.ReloadableConfig()
	rc.TrailingLogs = 1
	if err := a.Raft.ReloadConfig(rc); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := a.Apply([]byte(fmt.Sprintf("offline-%02d", i)), 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	proxy.reset()
	restored := &recordFSM{}
	transportNode(t, bDir, "node-b", bAddr, proxy.Addr(), false, restored)
	awaitTransport(t, "InstallSnapshot after offline compaction", func() bool {
		restored.mu.Lock()
		count := restored.restores
		restored.mu.Unlock()
		return count > 0 && restored.contains(secret) && restored.contains("offline-11")
	})
	if wire := proxy.captured(); len(wire) == 0 || bytes.Contains(wire, []byte(secret)) {
		t.Fatalf("snapshot secret was not protected on the wire (%d captured bytes)", len(wire))
	}
}

func TestRaftOfflineFollowerCatchesUpFromNewLeader(t *testing.T) {
	caCert, caKey := rafttest.NewCA(t)
	aDir, offlineDir := t.TempDir(), t.TempDir()
	rafttest.Issue(t, aDir, "node-a", caCert, caKey)
	rafttest.Issue(t, offlineDir, "offline", caCert, caKey)
	a := transportNode(t, aDir, "node-a", "127.0.0.1:0", "", true, &recordFSM{})
	if !waitForLeader(t, a, 10*time.Second) {
		t.Fatal("bootstrap did not elect leader")
	}
	offlineAddr := unusedAddress(t)
	before := &recordFSM{}
	offline := transportNode(t, offlineDir, "offline", offlineAddr, "", false, before)
	if err := a.Raft.AddNonvoter("offline", offline.LocalAddr(), 0, 10*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply([]byte("before-offline"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	awaitTransport(t, "initial follower catch-up", func() bool { return before.contains("before-offline") })
	if err := offline.Shutdown(); err != nil {
		t.Fatal(err)
	}

	var b *raftnode.Node
	for _, id := range []string{"node-b", "node-c"} {
		dir := t.TempDir()
		rafttest.Issue(t, dir, id, caCert, caKey)
		addr := unusedAddress(t)
		fsm := &recordFSM{}
		n := transportNode(t, dir, id, addr, "", false, fsm)
		if err := a.Raft.AddNonvoter(hraft.ServerID(id), n.LocalAddr(), 0, 10*time.Second).Error(); err != nil {
			t.Fatal(err)
		}
		awaitTransport(t, id+" replication", func() bool { return fsm.contains("before-offline") })
		if err := a.AddVoter(hraft.ServerID(id), n.LocalAddr(), 0, 10*time.Second).Error(); err != nil {
			t.Fatal(err)
		}
		if id == "node-b" {
			b = n
		}
	}
	if err := a.Raft.LeadershipTransferToServer("node-b", b.LocalAddr()).Error(); err != nil {
		t.Fatal(err)
	}
	if !waitForLeader(t, b, 10*time.Second) {
		t.Fatal("newly admitted node did not become leader")
	}
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply([]byte("after-failover"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	after := &recordFSM{}
	transportNode(t, offlineDir, "offline", offlineAddr, "", false, after)
	awaitTransport(t, "offline follower catch-up from newly admitted leader", func() bool {
		return after.contains("after-failover")
	})
}

func TestRaftTLSLeaderFailure(t *testing.T) {
	caCert, caKey := rafttest.NewCA(t)
	aDir := t.TempDir()
	rafttest.Issue(t, aDir, "node-a", caCert, caKey)
	a := transportNode(t, aDir, "node-a", "127.0.0.1:0", "", true, &recordFSM{})
	if !waitForLeader(t, a, 10*time.Second) {
		t.Fatal("bootstrap did not elect leader")
	}
	if _, err := a.Apply([]byte("before-failure"), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var followers []*raftnode.Node
	var states []*recordFSM
	for _, id := range []string{"node-b", "node-c"} {
		dir := t.TempDir()
		rafttest.Issue(t, dir, id, caCert, caKey)
		fsm := &recordFSM{}
		n := transportNode(t, dir, id, "127.0.0.1:0", "", false, fsm)
		if err := a.Raft.AddNonvoter(hraft.ServerID(id), n.LocalAddr(), 0, 5*time.Second).Error(); err != nil {
			t.Fatal(err)
		}
		awaitTransport(t, id+" catch-up", func() bool { return fsm.contains("before-failure") })
		if err := a.AddVoter(hraft.ServerID(id), n.LocalAddr(), 0, 5*time.Second).Error(); err != nil {
			t.Fatal(err)
		}
		followers, states = append(followers, n), append(states, fsm)
	}
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	var leader *raftnode.Node
	awaitTransport(t, "TLS election after leader shutdown", func() bool {
		for _, n := range followers {
			if n.IsLeader() {
				leader = n
				return true
			}
		}
		return false
	})
	if _, err := leader.Apply([]byte("after-failure"), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	awaitTransport(t, "post-failure replication", func() bool {
		return states[0].contains("after-failure") && states[1].contains("after-failure")
	})
}

func TestRaftSilentHandshakeDoesNotBlockMembersOrShutdown(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	n := transportNode(t, dir, "node-a", "127.0.0.1:0", "", false, noopFSM{})
	silent, err := net.DialTimeout("tcp", string(n.LocalAddr()), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	good, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", string(n.LocalAddr()),
		clientTLS(t, dir, "node-a", caCert, "node-a"))
	if err != nil {
		t.Fatalf("silent client blocked authenticated handshake: %v", err)
	}
	defer good.Close()
	if err := requestVote(good, "node-a"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- n.Shutdown() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silent handshake prevented shutdown")
	}
	if err := n.Shutdown(); err != nil {
		t.Fatalf("repeated shutdown: %v", err)
	}
	if err := silent.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("shutdown left unauthenticated socket open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("shutdown did not close the silent connection: %v", err)
	}
}

func TestNewFailureReleasesTransportAndStore(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", caCert, caKey)
	addr := unusedAddress(t)
	n := transportNode(t, dir, "node-a", addr, "", true, noopFSM{})
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	bad, err := raftnode.New(raftnode.Config{
		DataDir: dir, LocalID: "node-a", BindAddr: addr, Bootstrap: true,
		FSM: noopFSM{}, LogOutput: io.Discard,
	})
	if bad != nil {
		t.Cleanup(func() { _ = bad.Shutdown() })
	}
	if err == nil {
		t.Fatal("rebootstrapping existing state unexpectedly succeeded")
	}
	transportNode(t, dir, "node-a", addr, "", false, noopFSM{})
}

func transportNode(t *testing.T, dir, id, bind, advertise string, bootstrap bool, fsm hraft.FSM) *raftnode.Node {
	t.Helper()
	n, err := raftnode.New(raftnode.Config{
		DataDir: dir, LocalID: id, BindAddr: bind, AdvertiseAddr: advertise,
		Bootstrap: bootstrap, FSM: fsm, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	return n
}

func clientTLS(t *testing.T, dir, id string, rootsPEM []byte, serverName string) *tls.Config {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "node", id+".crt"), filepath.Join(dir, "node", id+".key"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootsPEM) {
		t.Fatal("invalid test CA")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serverName,
		NextProtos:           []string{"jaco-raft-v1"},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil },
	}
}

func alteredClientTLS(t *testing.T, dir, id string, caPEM, caKey []byte, change func(*x509.Certificate)) *tls.Config {
	t.Helper()
	cfg := clientTLS(t, dir, id, caPEM, "node-a")
	cert, err := cfg.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	authority, key, err := ca.ParseCA(caPEM, caKey)
	if err != nil {
		t.Fatal(err)
	}
	change(leaf)
	leaf.RawSubject = nil
	der, err := x509.CreateCertificate(rand.Reader, leaf, authority, leaf.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert.Certificate = [][]byte{der}
	cert.Leaf = nil
	return cfg
}

func requestVote(conn net.Conn, id string) error {
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		return err
	}
	req := &hraft.RequestVoteRequest{
		RPCHeader: hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte(id), Addr: []byte("127.0.0.1:1")},
		Term:      1,
	}
	if err := codec.NewEncoder(conn, &codec.MsgpackHandle{}).Encode(req); err != nil {
		return err
	}
	decoder := codec.NewDecoder(bufio.NewReader(conn), &codec.MsgpackHandle{})
	var rpcError string
	if err := decoder.Decode(&rpcError); err != nil {
		return err
	}
	if rpcError != "" {
		return fmt.Errorf("Raft RPC: %s", rpcError)
	}
	return decoder.Decode(&hraft.RequestVoteResponse{})
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func awaitTransport(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type recordFSM struct {
	mu       sync.Mutex
	commands []string
	restores int
}

func (f *recordFSM) Apply(log *hraft.Log) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, string(log.Data))
	return nil
}

func (f *recordFSM) contains(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.commands {
		if v == cmd {
			return true
		}
	}
	return false
}

func (f *recordFSM) Snapshot() (hraft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := json.Marshal(f.commands)
	return recordSnapshot(data), err
}

func (f *recordFSM) Restore(r io.ReadCloser) error {
	defer r.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := json.NewDecoder(r).Decode(&f.commands); err != nil {
		return err
	}
	f.restores++
	return nil
}

type recordSnapshot []byte

func (s recordSnapshot) Persist(sink hraft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (recordSnapshot) Release() {}

type wireProxy struct {
	listener net.Listener
	mu       sync.Mutex
	wire     bytes.Buffer
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func newWireProxy(t *testing.T, target string) *wireProxy {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &wireProxy{listener: lis, conns: make(map[net.Conn]struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			src, err := lis.Accept()
			if err != nil {
				return
			}
			dst, err := net.DialTimeout("tcp", target, time.Second)
			if err != nil {
				_ = src.Close()
				continue
			}
			p.mu.Lock()
			p.conns[src], p.conns[dst] = struct{}{}, struct{}{}
			p.mu.Unlock()
			p.wg.Add(2)
			copyConn := func(dst, src net.Conn) {
				defer p.wg.Done()
				defer func() {
					_ = src.Close()
					_ = dst.Close()
					p.mu.Lock()
					delete(p.conns, src)
					delete(p.conns, dst)
					p.mu.Unlock()
				}()
				_, _ = io.Copy(io.MultiWriter(dst, p), src)
			}
			go copyConn(dst, src)
			go copyConn(src, dst)
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		p.disconnect()
		p.wg.Wait()
	})
	return p
}

func (p *wireProxy) Addr() string { return p.listener.Addr().String() }

func (p *wireProxy) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wire.Write(data)
}

func (p *wireProxy) captured() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.wire.Bytes()...)
}

func (p *wireProxy) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wire.Reset()
}

func (p *wireProxy) disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.conns {
		_ = conn.Close()
	}
}
