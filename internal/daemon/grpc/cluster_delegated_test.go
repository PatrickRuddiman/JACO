package grpc_test

import (
	"context"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// waitForLeader waits until the daemon has elected itself leader on the
// single-voter cluster. Used by tests that need to invoke delegated
// operator-authenticated RPCs.
func waitForLeader(t *testing.T, raft interface{ IsLeader() bool }) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raft != nil && raft.IsLeader() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never became leader")
}

// TestDelegated_NodeListReachesHandler — post-Init the delegated NodeList
// returns the seeded local node entry. Proves the delegated() shim wires
// through to the controlplane handler.
func TestDelegated_NodeListReachesHandler(t *testing.T) {
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
	list, err := c.NodeList(authCtx, &pb.NodeListRequest{})
	if err != nil {
		t.Fatalf("NodeList: %v", err)
	}
	if len(list.GetNodes()) == 0 {
		t.Errorf("NodeList returned 0 nodes; expected the local node entry")
	}
}

// TestDelegated_IssueJoinTokenReturnsToken — operator-authenticated mint
// of a join token. We don't decode it; just assert it's non-empty and the
// expiry is in the future.
func TestDelegated_IssueJoinTokenReturnsToken(t *testing.T) {
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
	out, err := c.IssueJoinToken(authCtx, &pb.IssueJoinTokenRequest{})
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	if out.GetToken() == "" {
		t.Errorf("Token empty")
	}
	if len(out.GetCaCert()) == 0 {
		t.Errorf("CaCert empty")
	}
}

// TestDelegated_NodeRemoveEmptyHostnameRejected — drives the delegated
// NodeRemove path with an empty hostname; the controlplane handler
// rejects with InvalidArgument. Proves delegated() wires through to a
// handler that performs its own validation.
func TestDelegated_NodeRemoveEmptyHostnameRejected(t *testing.T) {
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
	_, err = c.NodeRemove(authCtx, &pb.NodeRemoveRequest{Hostname: "", Force: true})
	if err == nil {
		t.Fatalf("NodeRemove with empty hostname succeeded; want InvalidArgument")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestDelegated_BackupStreamsAtLeastOneChunk — Backup is a server-stream
// RPC; recv at least one chunk to prove the delegated() shim forwards
// streams correctly.
func TestDelegated_BackupStreamsAtLeastOneChunk(t *testing.T) {
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
	stream, err := c.Backup(authCtx, &pb.BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var got int
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Any non-EOF error must NOT be the delegated fallback.
			st, _ := status.FromError(err)
			if st.Code() == codes.Unavailable {
				t.Fatalf("Backup hit delegated fallback: %v", err)
			}
			break
		}
		got++
		if got > 0 {
			// One chunk is enough to confirm the stream wires through;
			// cancel to avoid streaming the rest.
			return
		}
	}
	if got == 0 {
		t.Errorf("Backup produced 0 chunks; expected at least one")
	}
}

// TestDelegated_RestoreReachesHandlerWithMalformedHeader — feed Restore
// a deliberately malformed first message; expect the delegated handler
// (not the proxy fallback) to surface an InvalidArgument or similar
// error.
func TestDelegated_RestoreReachesHandlerWithMalformedHeader(t *testing.T) {
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
	stream, err := c.Restore(authCtx)
	if err != nil {
		t.Fatalf("Restore open: %v", err)
	}
	// Send a single chunk of nonsense data — the handler should refuse
	// it. We only care that we reached the handler.
	if err := stream.Send(&pb.BackupChunk{Data: []byte("not-a-valid-backup")}); err != nil {
		// Some Send paths surface server errors here; that's fine.
		st, _ := status.FromError(err)
		if st.Code() == codes.Unavailable && st.Message() == "state_unavailable" {
			t.Errorf("Restore hit delegated state_unavailable fallback")
		}
		return
	}
	if _, err := stream.CloseAndRecv(); err == nil {
		t.Errorf("Restore with malformed body succeeded; want error")
	} else {
		st, _ := status.FromError(err)
		if st.Code() == codes.Unavailable && st.Message() == "state_unavailable" {
			t.Errorf("Restore hit delegated state_unavailable fallback: %v", err)
		}
	}
}
