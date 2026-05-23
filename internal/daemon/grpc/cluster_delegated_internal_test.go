package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestClusterDelegated_NilStateReturnsUnavailable — pre-OpenRaft the
// daemon's State() returns nil. delegated() must surface a
// state_unavailable error rather than nil-deref the controlplane shim.
func TestClusterDelegated_NilStateReturnsUnavailable(t *testing.T) {
	c := &clusterServer{server: &Server{}}
	_, err := c.NodeList(context.Background(), &pb.NodeListRequest{})
	if err == nil {
		t.Fatalf("NodeList with nil state: nil err")
	}
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

// TestClusterDelegated_NilRaftReturnsUnavailable — State present but
// raft handle isn't; delegated() returns raft_unavailable.
func TestClusterDelegated_NilRaftReturnsUnavailable(t *testing.T) {
	s := &Server{}
	s.raftMu.Lock()
	s.state = state.New(watch.NewRegistry())
	s.raftMu.Unlock()

	c := &clusterServer{server: s}
	_, err := c.NodeList(context.Background(), &pb.NodeListRequest{})
	if err == nil {
		t.Fatalf("NodeList with nil raft: nil err")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

// TestClusterDelegated_NodeRemoveNilState — sister test for the
// NodeRemove forwarder.
func TestClusterDelegated_NodeRemoveNilState(t *testing.T) {
	c := &clusterServer{server: &Server{}}
	_, err := c.NodeRemove(context.Background(), &pb.NodeRemoveRequest{Hostname: "x"})
	if err == nil {
		t.Fatalf("NodeRemove with nil state: nil err")
	}
}

// TestClusterDelegated_IssueJoinTokenNilState — same for
// IssueJoinToken.
func TestClusterDelegated_IssueJoinTokenNilState(t *testing.T) {
	c := &clusterServer{server: &Server{}}
	_, err := c.IssueJoinToken(context.Background(), &pb.IssueJoinTokenRequest{})
	if err == nil {
		t.Fatalf("IssueJoinToken with nil state: nil err")
	}
}

// TestEffectiveHostname_OverrideAndDefault — when the hostname
// override is set, effectiveHostname returns it; otherwise it falls
// through to os.Hostname (which always succeeds on test hosts).
func TestEffectiveHostname_OverrideAndDefault(t *testing.T) {
	c := &clusterServer{hostname: "explicit"}
	got, err := c.effectiveHostname()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "explicit" {
		t.Errorf("override path: got %q", got)
	}

	c2 := &clusterServer{} // empty hostname → os.Hostname()
	got2, err := c2.effectiveHostname()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got2 == "" {
		t.Errorf("os.Hostname returned empty")
	}
}

// TestEffectiveBindAddr_OverrideAndDefault — same shape.
func TestEffectiveBindAddr_OverrideAndDefault(t *testing.T) {
	c := &clusterServer{bindAddr: "10.0.0.1:1234"}
	if got := c.effectiveBindAddr(); got != "10.0.0.1:1234" {
		t.Errorf("override path: got %q", got)
	}
	c2 := &clusterServer{}
	if got := c2.effectiveBindAddr(); got != "127.0.0.1:0" {
		t.Errorf("default path: got %q, want 127.0.0.1:0", got)
	}
}
