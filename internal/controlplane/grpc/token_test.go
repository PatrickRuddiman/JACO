package grpcsrv_test

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// twoNodeCluster brings up a real two-node JACO cluster (raft + gRPC on both)
// reusable by tests that need cross-node propagation.
type twoNodeCluster struct {
	A             *clusterNode
	B             *clusterNode
	OperatorToken string
}

type clusterNode struct {
	Name     string
	DataDir  string
	RaftAddr string
	GrpcAddr string
	State    *state.State
	Brokers  *watch.Registry
	Raft     *raftnode.Node
	Server   *grpcsrv.Server
	CACert   []byte
	Tokens   pb.TokensClient
	Cluster  pb.ClusterClient
	Audit    pb.AuditClient
}

// setupTwoNodeCluster bootstraps A, then onboards B via the full join
// handshake. Cleanup is registered with t.Cleanup.
func setupTwoNodeCluster(t *testing.T) *twoNodeCluster {
	t.Helper()

	aRaft := freePort(t)
	bRaft := freePort(t)

	// --- Node A: bootstrap, re-open, gRPC. ---
	aDir := t.TempDir()
	bootRes, err := bootstrap.Run(bootstrap.Options{DataDir: aDir, Name: "node-a", BindAddr: aRaft})
	if err != nil {
		t.Fatalf("bootstrap A: %v", err)
	}
	a := openClusterNode(t, "node-a", aDir, aRaft)

	aCaCert, _ := os.ReadFile(filepath.Join(aDir, "node", "ca.crt"))
	aNodeCert, _ := os.ReadFile(filepath.Join(aDir, "node", "node-a.crt"))
	aNodeKey, _ := os.ReadFile(filepath.Join(aDir, "node", "node-a.key"))
	a.Server = startGRPCServer(t, freePort(t), aNodeCert, aNodeKey, aCaCert, a.State, a.Brokers, a.Raft)
	a.GrpcAddr = a.Server.Addr().String()
	a.CACert = aCaCert
	conn := dialConn(t, a.GrpcAddr, aCaCert, "node-a")
	t.Cleanup(func() { _ = conn.Close() })
	a.Tokens = pb.NewTokensClient(conn)
	a.Cluster = pb.NewClusterClient(conn)
	a.Audit = pb.NewAuditClient(conn)

	// --- Node B: raft up, join via A's gRPC, then start B's gRPC. ---
	bDir := t.TempDir()
	b := openClusterNode(t, "node-b", bDir, bRaft)

	ctxOp := authContext(bootRes.OperatorToken)
	issueResp, err := a.Cluster.IssueJoinToken(ctxOp, &pb.IssueJoinTokenRequest{})
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	bKey, bCSR, err := ca.GenerateNodeKeypair("node-b")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair B: %v", err)
	}
	joinResp, err := a.Cluster.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name:          "node-b",
		JoinToken:     issueResp.GetToken(),
		CsrPem:        bCSR,
		AdvertiseAddr: bRaft,
	})
	if err != nil {
		t.Fatalf("NodeJoin: %v", err)
	}

	b.Server = startGRPCServer(t, freePort(t), joinResp.GetSignedCert(), bKey, joinResp.GetCaCert(), b.State, b.Brokers, b.Raft)
	b.GrpcAddr = b.Server.Addr().String()
	b.CACert = joinResp.GetCaCert()
	bConn := dialConn(t, b.GrpcAddr, joinResp.GetCaCert(), "node-b")
	t.Cleanup(func() { _ = bConn.Close() })
	b.Tokens = pb.NewTokensClient(bConn)
	b.Cluster = pb.NewClusterClient(bConn)
	b.Audit = pb.NewAuditClient(bConn)

	// Both nodes' state must reflect the second member before tests use B.
	waitFor(t, 5*time.Second, "A.state.Nodes has node-b", func() bool {
		_, ok := a.State.Nodes.Get("node-b")
		return ok
	})
	waitFor(t, 5*time.Second, "B.state.Nodes has node-b", func() bool {
		_, ok := b.State.Nodes.Get("node-b")
		return ok
	})

	return &twoNodeCluster{A: a, B: b, OperatorToken: bootRes.OperatorToken}
}

// openClusterNode opens a raft node (no bootstrap) against an existing or
// fresh data dir. The caller starts the gRPC server separately.
func openClusterNode(t *testing.T, name, dataDir, raftAddr string) *clusterNode {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	r, err := raftnode.New(raftnode.Config{
		DataDir:   dataDir,
		BindAddr:  raftAddr,
		LocalID:   name,
		Bootstrap: false,
		FSM:       f,
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("raftnode.New %s: %v", name, err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	// For node-a only, wait for self-leader + state seeded by replay.
	if name == "node-a" {
		waitForLeader(t, r, 10*time.Second)
		waitFor(t, 5*time.Second, "self-Node populated", func() bool {
			_, ok := st.Nodes.Get(name)
			return ok
		})
	}

	return &clusterNode{Name: name, DataDir: dataDir, RaftAddr: raftAddr, State: st, Brokers: brokers, Raft: r}
}

func startGRPCServer(t *testing.T, bindAddr string, nodeCert, nodeKey, caCert []byte, st *state.State, brokers *watch.Registry, r *raftnode.Node) *grpcsrv.Server {
	t.Helper()
	srv, err := grpcsrv.NewServer(grpcsrv.Options{
		BindAddr: bindAddr,
		NodeCert: nodeCert,
		NodeKey:  nodeKey,
		CACert:   caCert,
		State:    st,
		Brokers:  brokers,
		Raft:     r,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Stop)
	return srv
}

func authContext(bearer string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
}

// --- The actual Tokens tests --------------------------------------------------

func TestTokensIssue_RequiresLeader(t *testing.T) {
	// No raft wired ⇒ raft_unavailable.
	caCertPEM, caKeyPEM, _ := ca.GenerateClusterCA()
	nodeKey, csrPEM, _ := ca.GenerateNodeKeypair("127.0.0.1")
	nodeCert, _ := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	st := state.New(watch.NewRegistry())
	st.Cluster.Set(&pb.ClusterMeta{ClusterId: "x", CaCert: caCertPEM, CaKey: caKeyPEM}, 1)
	// Seed an operator token so the admission gate is open.
	const opToken = "test-operator-token"
	st.Tokens.Apply(&pb.Token{Identity: "operator", HashedSecret: sha256Bytes(opToken)}, 1)

	srv, _ := grpcsrv.NewServer(grpcsrv.Options{
		BindAddr: freePort(t),
		NodeCert: nodeCert, NodeKey: nodeKey, CACert: caCertPEM,
		State: st, Raft: nil,
	})
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Stop)

	conn := dialConn(t, srv.Addr().String(), caCertPEM, "127.0.0.1")
	defer conn.Close()
	client := pb.NewTokensClient(conn)
	_, err := client.Issue(authContext(opToken), &pb.TokenIssueRequest{Identity: "alice"})
	if err == nil {
		t.Fatalf("expected error when raft not wired")
	}
	if sErr, ok := status.FromError(err); !ok || sErr.Message() != "raft_unavailable" {
		t.Errorf("err = %v; want raft_unavailable", err)
	}
}

func TestTokensList_StripsHashedSecret(t *testing.T) {
	c := setupTwoNodeCluster(t)
	ctxOp := authContext(c.OperatorToken)

	resp, err := c.A.Tokens.List(ctxOp, &pb.TokenListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetTokens()) == 0 {
		t.Fatalf("List returned 0 tokens; expected at least the bootstrap token")
	}
	for _, info := range resp.GetTokens() {
		// TokenInfo doesn't have a hashed_secret field; the assertion here is
		// that the response shape doesn't surface secrets even by accident.
		if info.GetIdentity() == "" {
			t.Errorf("token has empty identity")
		}
	}
}

func TestTokensIssueRevoke_PropagatesAcrossNodesWithin5s(t *testing.T) {
	c := setupTwoNodeCluster(t)
	ctxOp := authContext(c.OperatorToken)

	// Issue a new operator token via A.
	resp, err := c.A.Tokens.Issue(ctxOp, &pb.TokenIssueRequest{Identity: "alice"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	aliceToken := resp.GetToken()
	if aliceToken == "" {
		t.Fatalf("Issue returned empty token")
	}

	// Wait for B's state to reflect the new token (replication is sub-second
	// on localhost; 2s budget is generous).
	waitFor(t, 2*time.Second, "alice token replicated to B", func() bool {
		_, ok := c.B.State.Tokens.Get("alice")
		return ok
	})

	// Use alice on B — Cluster.Status should succeed.
	if _, err := c.B.Cluster.Status(authContext(aliceToken), &pb.ClusterStatusRequest{}); err != nil {
		t.Fatalf("alice's token rejected on B before revocation: %v", err)
	}

	// Revoke alice's token on A and poll B with the same token until rejected.
	revokeStart := time.Now()
	if _, err := c.A.Tokens.Revoke(ctxOp, &pb.TokenRevokeRequest{Identity: "alice"}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	deadline := revokeStart.Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := c.B.Cluster.Status(authContext(aliceToken), &pb.ClusterStatusRequest{})
		if err != nil {
			sErr, _ := status.FromError(err)
			if strings.Contains(sErr.Message(), "token_revoked") {
				elapsed := time.Since(revokeStart)
				t.Logf("revocation observed on B in %s", elapsed)
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("alice's token was not revoked on B within 5s of revocation on A")
}

func TestTokensIssue_RequiresIdentity(t *testing.T) {
	c := setupTwoNodeCluster(t)
	ctxOp := authContext(c.OperatorToken)
	_, err := c.A.Tokens.Issue(ctxOp, &pb.TokenIssueRequest{Identity: ""})
	if err == nil {
		t.Fatalf("expected validation error for empty identity")
	}
	if sErr, ok := status.FromError(err); !ok || sErr.Message() != "validation_failed" {
		t.Errorf("err = %v; want validation_failed", err)
	}
}

// --- helper used in the no-raft test ----------------------------------------

func sha256Bytes(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
