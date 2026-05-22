package grpcsrv_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// freePort grabs an unused TCP port on 127.0.0.1, closes the listener, and
// returns "127.0.0.1:<port>". There's a brief race where another process could
// grab it before we re-bind; acceptable for tests.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestNodeJoin_TwoNodeClusterAndSingleUseToken(t *testing.T) {
	// Pre-allocate ports so the bootstrap node's recorded address survives the
	// reopen-after-bootstrap round-trip.
	aRaftAddr := freePort(t)
	bRaftAddr := freePort(t)

	// 1. Bootstrap node A in its own data dir on aRaftAddr.
	aDir := t.TempDir()
	bootRes, err := bootstrap.Run(bootstrap.Options{
		DataDir:  aDir,
		Name:     "node-a",
		BindAddr: aRaftAddr,
	})
	if err != nil {
		t.Fatalf("bootstrap node-a: %v", err)
	}
	operatorToken := bootRes.OperatorToken

	// 2. Re-open A's raft (post-bootstrap; not Bootstrap=true this time) with
	//    a fresh state + fsm that replays the log.
	aBrokers := watch.NewRegistry()
	aState := state.New(aBrokers)
	aFSM := fsm.New(aState, aBrokers)
	aRaft, err := raftnode.New(raftnode.Config{
		DataDir:   aDir,
		BindAddr:  aRaftAddr,
		LocalID:   "node-a",
		Bootstrap: false,
		FSM:       aFSM,
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("re-open A raft: %v", err)
	}
	t.Cleanup(func() { _ = aRaft.Shutdown() })

	// Wait for leadership + bootstrap log replay (the ClusterInit Apply lands
	// during this window, which writes both ClusterMeta and the self-Node).
	waitForLeader(t, aRaft, 10*time.Second)
	waitFor(t, 5*time.Second, "self-node in state", func() bool {
		_, ok := aState.Nodes.Get("node-a")
		return ok
	})

	// 3. Start the gRPC server for A on a free port.
	aGrpcAddr := freePort(t)
	aNodeCert, _ := os.ReadFile(filepath.Join(aDir, "node", "node-a.crt"))
	aNodeKey, _ := os.ReadFile(filepath.Join(aDir, "node", "node-a.key"))
	aCACert, _ := os.ReadFile(filepath.Join(aDir, "node", "ca.crt"))
	srv, err := grpcsrv.NewServer(grpcsrv.Options{
		BindAddr: aGrpcAddr,
		NodeCert: aNodeCert,
		NodeKey:  aNodeKey,
		CACert:   aCACert,
		State:    aState,
		Raft:     aRaft,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Stop)

	// 4. Start node B's raft on bRaftAddr (no bootstrap; waits to be added).
	bDir := t.TempDir()
	bBrokers := watch.NewRegistry()
	bState := state.New(bBrokers)
	bFSM := fsm.New(bState, bBrokers)
	bRaft, err := raftnode.New(raftnode.Config{
		DataDir:   bDir,
		BindAddr:  bRaftAddr,
		LocalID:   "node-b",
		Bootstrap: false,
		FSM:       bFSM,
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("start node-b raft: %v", err)
	}
	t.Cleanup(func() { _ = bRaft.Shutdown() })

	// 5. Dial A with the CA pinned. ServerName matches the cert's DNS SAN
	//    (the node hostname), which is what bootstrap signed.
	client := dialClusterClient(t, srv.Addr().String(), aCACert, "node-a")

	// 6. IssueJoinToken (operator-authenticated).
	ctxOp := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+operatorToken)
	issueResp, err := client.IssueJoinToken(ctxOp, &pb.IssueJoinTokenRequest{})
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	joinToken := issueResp.GetToken()
	if joinToken == "" {
		t.Fatalf("empty join token returned")
	}

	// 7. Build B's CSR and call NodeJoin (unauthenticated).
	_, bCSR, err := ca.GenerateNodeKeypair("node-b")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	joinResp, err := client.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name:          "node-b",
		JoinToken:     joinToken,
		CsrPem:        bCSR,
		AdvertiseAddr: bRaftAddr,
	})
	if err != nil {
		t.Fatalf("NodeJoin: %v", err)
	}
	if len(joinResp.GetSignedCert()) == 0 {
		t.Errorf("NodeJoin returned empty signed_cert")
	}
	if len(joinResp.GetCaCert()) == 0 {
		t.Errorf("NodeJoin returned empty ca_cert")
	}

	// 8. NodeList must surface both A and B within 5s of replication.
	var listResp *pb.NodeListResponse
	waitFor(t, 5*time.Second, "NodeList returns 2", func() bool {
		listResp, err = client.NodeList(ctxOp, &pb.NodeListRequest{})
		return err == nil && len(listResp.GetNodes()) == 2
	})
	if listResp == nil || len(listResp.GetNodes()) != 2 {
		t.Fatalf("NodeList returned %d nodes, want 2: %+v / err=%v", len(listResp.GetNodes()), listResp.GetNodes(), err)
	}
	hostnames := make(map[string]bool)
	for _, n := range listResp.GetNodes() {
		hostnames[n.GetHostname()] = true
	}
	if !hostnames["node-a"] || !hostnames["node-b"] {
		t.Errorf("expected node-a and node-b in list, got %v", hostnames)
	}

	// 9. AC: replaying the same join token must fail with join_token_consumed.
	_, csr2, _ := ca.GenerateNodeKeypair("node-c")
	_, err = client.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name: "node-c", JoinToken: joinToken, CsrPem: csr2,
		AdvertiseAddr: freePort(t),
	})
	if err == nil {
		t.Fatalf("expected error on token replay, got nil")
	}
	if !strings.Contains(err.Error(), "join_token_consumed") {
		t.Errorf("error %q does not contain 'join_token_consumed'", err.Error())
	}

	// 10. Unknown token also fails.
	_, csr3, _ := ca.GenerateNodeKeypair("node-d")
	_, err = client.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name: "node-d", JoinToken: "garbage", CsrPem: csr3,
		AdvertiseAddr: freePort(t),
	})
	if err == nil || !strings.Contains(err.Error(), "join_token_invalid") {
		t.Errorf("expected join_token_invalid, got %v", err)
	}
}

func TestNodeRemove_EvictsFromRaftAndState(t *testing.T) {
	// Reuse the join flow: bootstrap A, join B, then remove B.
	aRaftAddr := freePort(t)
	bRaftAddr := freePort(t)
	aDir := t.TempDir()
	bootRes, err := bootstrap.Run(bootstrap.Options{
		DataDir: aDir, Name: "node-a", BindAddr: aRaftAddr,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	aBrokers := watch.NewRegistry()
	aState := state.New(aBrokers)
	aFSM := fsm.New(aState, aBrokers)
	aRaft, err := raftnode.New(raftnode.Config{
		DataDir: aDir, BindAddr: aRaftAddr, LocalID: "node-a",
		Bootstrap: false, FSM: aFSM, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("re-open A raft: %v", err)
	}
	t.Cleanup(func() { _ = aRaft.Shutdown() })
	waitForLeader(t, aRaft, 10*time.Second)
	waitFor(t, 5*time.Second, "self-node", func() bool { _, ok := aState.Nodes.Get("node-a"); return ok })

	aNodeCert, _ := os.ReadFile(filepath.Join(aDir, "node", "node-a.crt"))
	aNodeKey, _ := os.ReadFile(filepath.Join(aDir, "node", "node-a.key"))
	aCACert, _ := os.ReadFile(filepath.Join(aDir, "node", "ca.crt"))
	srv, err := grpcsrv.NewServer(grpcsrv.Options{
		BindAddr: freePort(t),
		NodeCert: aNodeCert, NodeKey: aNodeKey, CACert: aCACert,
		State: aState, Raft: aRaft,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Stop)

	bDir := t.TempDir()
	bRaft, err := raftnode.New(raftnode.Config{
		DataDir: bDir, BindAddr: bRaftAddr, LocalID: "node-b",
		Bootstrap: false,
		FSM:       fsm.New(state.New(watch.NewRegistry()), watch.NewRegistry()),
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("start node-b raft: %v", err)
	}
	t.Cleanup(func() { _ = bRaft.Shutdown() })

	client := dialClusterClient(t, srv.Addr().String(), aCACert, "node-a")
	ctxOp := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bootRes.OperatorToken)

	issueResp, _ := client.IssueJoinToken(ctxOp, &pb.IssueJoinTokenRequest{})
	_, bCSR, _ := ca.GenerateNodeKeypair("node-b")
	_, err = client.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name: "node-b", JoinToken: issueResp.GetToken(), CsrPem: bCSR,
		AdvertiseAddr: bRaftAddr,
	})
	if err != nil {
		t.Fatalf("NodeJoin: %v", err)
	}
	waitFor(t, 5*time.Second, "2 nodes", func() bool {
		r, _ := client.NodeList(ctxOp, &pb.NodeListRequest{})
		return r != nil && len(r.GetNodes()) == 2
	})

	// Remove B.
	if _, err := client.NodeRemove(ctxOp, &pb.NodeRemoveRequest{Hostname: "node-b", Force: true}); err != nil {
		t.Fatalf("NodeRemove: %v", err)
	}
	waitFor(t, 5*time.Second, "1 node", func() bool {
		r, _ := client.NodeList(ctxOp, &pb.NodeListRequest{})
		return r != nil && len(r.GetNodes()) == 1
	})
	r, _ := client.NodeList(ctxOp, &pb.NodeListRequest{})
	if len(r.GetNodes()) != 1 || r.GetNodes()[0].GetHostname() != "node-a" {
		t.Errorf("after NodeRemove: %+v", r.GetNodes())
	}
}

func TestNodeJoin_RequiresLeader_NoRaftReturnsError(t *testing.T) {
	// Build a minimal server with State + a Cluster CA but no Raft → NodeJoin
	// fails fast with raft_unavailable / no_leader.
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	nodeKey, csrPEM, _ := ca.GenerateNodeKeypair("127.0.0.1")
	nodeCert, _ := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	st := state.New(watch.NewRegistry())
	st.Cluster.Set(&pb.ClusterMeta{ClusterId: "x", CaCert: caCertPEM, CaKey: caKeyPEM}, 1)
	srv, err := grpcsrv.NewServer(grpcsrv.Options{
		BindAddr: freePort(t),
		NodeCert: nodeCert, NodeKey: nodeKey, CACert: caCertPEM,
		State: st, Raft: nil,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Stop)

	conn := dialConn(t, srv.Addr().String(), caCertPEM, "127.0.0.1")
	defer conn.Close()
	_, csr2, _ := ca.GenerateNodeKeypair("node-z")
	_, err = pb.NewClusterClient(conn).NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name: "node-z", JoinToken: "anything", CsrPem: csr2, AdvertiseAddr: "127.0.0.1:1",
	})
	if err == nil {
		t.Fatalf("expected error when raft is not wired")
	}
	if sErr, ok := status.FromError(err); ok {
		if sErr.Message() != "raft_unavailable" {
			t.Errorf("message = %q, want raft_unavailable", sErr.Message())
		}
	}
}

// --- helpers -----------------------------------------------------------------

func dialClusterClient(t *testing.T, addr string, caPEM []byte, serverName string) pb.ClusterClient {
	t.Helper()
	conn := dialConn(t, addr, caPEM, serverName)
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewClusterClient(conn)
}

func dialConn(t *testing.T, addr string, caPEM []byte, serverName string) *grpc.ClientConn {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	creds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: serverName})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	// Block until the server is reachable.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn.GetState().String() != "TRANSIENT_FAILURE" {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	return conn
}

func waitForLeader(t *testing.T, r *raftnode.Node, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.IsLeader() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never became leader; state=%v", r.Raft.State())
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitFor(%s) timed out after %s", what, timeout)
}

// silence unused
var _ = fmt.Sprintf
var _ = hraft.ServerID("x")
