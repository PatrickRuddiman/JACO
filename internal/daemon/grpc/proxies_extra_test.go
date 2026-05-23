package grpc_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestProxies_TokensIssueReachesHandler — Tokens.Issue proxies through
// after Init. We don't decode the token; just assert the call lands in
// the controlplane handler and returns the cleartext token.
func TestProxies_TokensIssueReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	tokens := pb.NewTokensClient(conn)
	out, err := tokens.Issue(authCtx, &pb.TokenIssueRequest{Identity: "alice"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if out.GetToken() == "" {
		t.Errorf("Issue returned empty token")
	}
	if out.GetIdentity() != "alice" {
		t.Errorf("Issue.Identity = %q, want alice", out.GetIdentity())
	}
}

// TestProxies_TokensRevokeReachesHandler — Tokens.Revoke is idempotent;
// a "ghost" identity still returns success after reaching the handler.
func TestProxies_TokensRevokeReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	tokens := pb.NewTokensClient(conn)
	if _, err := tokens.Revoke(authCtx, &pb.TokenRevokeRequest{Identity: "ghost"}); err != nil {
		t.Fatalf("Revoke ghost: %v", err)
	}
}

// TestProxies_DeployRollbackReachesHandler — Rollback on a non-existent
// deployment hits the handler (not the proxy fallback) and returns
// NotFound or similar, not Unavailable/state_unavailable.
func TestProxies_DeployRollbackReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	deploy := pb.NewDeployClient(conn)
	_, err = deploy.Rollback(authCtx, &pb.RollbackRequest{Deployment: "ghost"})
	if err == nil {
		t.Fatalf("Rollback on ghost succeeded; want error")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.Unavailable && contains(st.Message(), "state_unavailable") {
		t.Errorf("Rollback hit proxy fallback: %v", err)
	}
}

// TestProxies_DeployDeleteReachesHandler — Delete a non-existent
// deployment is idempotent; the handler accepts the request.
func TestProxies_DeployDeleteReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	deploy := pb.NewDeployClient(conn)
	// The handler may return NotFound or success. Either way we want to
	// confirm the proxy fallback is NOT hit.
	_, err = deploy.Delete(authCtx, &pb.DeleteRequest{Deployment: "ghost"})
	if err != nil {
		st, _ := status.FromError(err)
		if st.Code() == codes.Unavailable && contains(st.Message(), "state_unavailable") {
			t.Errorf("Delete hit proxy fallback: %v", err)
		}
	}
}

// TestProxies_DeployStatusReachesHandler — Status on a ghost deployment
// goes through the proxy and reaches the handler; result may be empty
// but not Unavailable.
func TestProxies_DeployStatusReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	deploy := pb.NewDeployClient(conn)
	_, err = deploy.Status(authCtx, &pb.DeployStatusRequest{DeploymentFilter: "ghost"})
	if err != nil {
		st, _ := status.FromError(err)
		if st.Code() == codes.Unavailable && contains(st.Message(), "state_unavailable") {
			t.Errorf("Status hit proxy fallback: %v", err)
		}
	}
}

// TestProxies_WatchSubscribeReachesHandler — Watch.Subscribe is a
// server-streaming RPC. We open the stream, immediately cancel, and
// confirm the handler accepted the open (i.e. no state_unavailable
// fallback).
func TestProxies_WatchSubscribeReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	ctx, cancel := context.WithCancel(withOperatorAuth(context.Background(), resp.GetOperatorToken()))
	defer cancel()
	watch := pb.NewWatchClient(conn)
	stream, err := watch.Subscribe(ctx, &pb.SubscribeRequest{
		EntityTypes: []string{"nodes"},
	})
	if err != nil {
		t.Fatalf("Subscribe open: %v", err)
	}
	// Read one frame with a short deadline; the daemon may send a
	// snapshot prefix immediately (depending on broker semantics) or
	// nothing. Either way we want to confirm the stream wasn't
	// rejected by the proxy.
	doneCh := make(chan error, 1)
	go func() { _, err := stream.Recv(); doneCh <- err }()
	select {
	case err := <-doneCh:
		if err != nil {
			st, _ := status.FromError(err)
			if st.Code() == codes.Unavailable && contains(st.Message(), "state_unavailable") {
				t.Errorf("Subscribe hit proxy fallback: %v", err)
			}
		}
	case <-time.After(200 * time.Millisecond):
		// no event yet — that's fine, just cancel.
	}
}

// TestProxies_AuditQueryReachesHandler — Audit.Query is server-streaming;
// the call should reach the handler whether or not events exist.
func TestProxies_AuditQueryReachesHandler(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	waitForOperatorToken(t, s)
	waitForLeader(t, s.Raft())

	ctx, cancel := context.WithCancel(withOperatorAuth(context.Background(), resp.GetOperatorToken()))
	defer cancel()
	audit := pb.NewAuditClient(conn)
	stream, err := audit.Query(ctx, &pb.AuditQueryRequest{})
	if err != nil {
		t.Fatalf("Query open: %v", err)
	}
	doneCh := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				doneCh <- err
				return
			}
		}
	}()
	select {
	case err := <-doneCh:
		if err != nil {
			st, _ := status.FromError(err)
			if st.Code() == codes.Unavailable && contains(st.Message(), "state_unavailable") {
				t.Errorf("Query hit proxy fallback: %v", err)
			}
		}
	case <-time.After(300 * time.Millisecond):
		// drained or quiet — proves the call wasn't rejected.
	}
}

// TestServer_SocketPath — trivial accessor; assert it round-trips.
func TestServer_SocketPath(t *testing.T) {
	_, s := startServerWithDataDir(t, t.TempDir())
	if got := s.SocketPath(); got == "" {
		t.Errorf("SocketPath empty after construction")
	}
}
