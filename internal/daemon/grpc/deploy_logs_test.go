package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestStreamLocalLogs_NilDockerReturnsUnavailable — when the daemon
// was constructed without a Docker handle, streamLocalLogs refuses
// rather than panicking.
func TestStreamLocalLogs_NilDockerReturnsUnavailable(t *testing.T) {
	s := &Server{}
	err := s.streamLocalLogs(&pb.LogsRequest{Deployment: "smoke"}, nil)
	if err == nil {
		t.Fatalf("streamLocalLogs with nil docker returned nil err")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

// TestStreamLocalLogs_EmptyDeploymentReturnsInvalidArgument — the
// handler rejects empty deployment names before iterating state.
func TestStreamLocalLogs_EmptyDeploymentReturnsInvalidArgument(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	s := &Server{
		docker:  noopDocker{},
		state:   st,
		cluster: &clusterServer{hostname: "h"},
	}
	err := s.streamLocalLogs(&pb.LogsRequest{Deployment: ""}, nil)
	if err == nil {
		t.Fatalf("empty deployment: err = nil")
	}
	se, _ := status.FromError(err)
	if se.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", se.Code())
	}
}

// TestStreamLocalLogs_NoMatchingReplicasReturnsNil — when no
// ReplicaDesired entry lives on the local host for the requested
// deployment, streamLocalLogs returns nil (no log streams to fan out).
func TestStreamLocalLogs_NoMatchingReplicasReturnsNil(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	// Plant a replica on a DIFFERENT host so the filter excludes it.
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "smoke-web-0", Deployment: "smoke", Service: "web", Host: "other-host",
	}, 1)

	s := &Server{
		docker:  noopDocker{},
		state:   st,
		cluster: &clusterServer{hostname: "h"},
	}
	err := s.streamLocalLogs(&pb.LogsRequest{Deployment: "smoke"}, nil)
	if err != nil {
		t.Errorf("no-matching-replicas: err = %v, want nil", err)
	}
}

// TestStreamLocalLogs_NilStateReturnsUnavailable — the state-nil guard
// fires before the hostname-resolve / replica-list path.
func TestStreamLocalLogs_NilStateReturnsUnavailable(t *testing.T) {
	s := &Server{docker: noopDocker{}}
	err := s.streamLocalLogs(&pb.LogsRequest{Deployment: "x"}, nil)
	if err == nil {
		t.Fatalf("nil state: err = nil")
	}
	se, _ := status.FromError(err)
	if se.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", se.Code())
	}
}

// TestMutexSender_ContextAndSend — mutexSender wraps ctx + send and
// implements the logsSender interface used by the multi-host fanout
// in streamDeploymentLogs.
func TestMutexSender_ContextAndSend(t *testing.T) {
	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")
	called := false
	m := mutexSender{
		ctx:  ctx,
		send: func(*pb.LogLine) error { called = true; return nil },
	}
	if m.Context() != ctx {
		t.Errorf("Context() not propagated")
	}
	if err := m.Send(&pb.LogLine{}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if !called {
		t.Errorf("Send callback not invoked")
	}
}

// noopDocker satisfies dockerx.Docker without exercising any of its
// methods. The streamLocalLogs paths under test all return before any
// docker method is called.
type noopDocker struct {
	dockerx.Docker
}
