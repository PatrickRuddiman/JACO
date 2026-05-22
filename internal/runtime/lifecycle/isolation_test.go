package lifecycle_test

import (
	"errors"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newStateWithNode(name string, status pb.NodeStatus) *state.State {
	st := state.New(watch.NewRegistry())
	st.Nodes.Apply(&pb.Node{Hostname: name, Status: status}, 1)
	return st
}

func TestCheckIsolationAvailable_ReadyNodePasses(t *testing.T) {
	st := newStateWithNode("node-a", pb.NodeStatus_NODE_STATUS_READY)
	if err := lifecycle.CheckIsolationAvailable(st, "node-a"); err != nil {
		t.Errorf("ready node should pass: %v", err)
	}
}

func TestCheckIsolationAvailable_IsolationUnavailableRejects(t *testing.T) {
	// The AC: refuse to call ContainerCreate when local Node has
	// status=isolation_unavailable.
	st := newStateWithNode("node-a", pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE)
	err := lifecycle.CheckIsolationAvailable(st, "node-a")
	if !errors.Is(err, lifecycle.ErrIsolationUnavailable) {
		t.Errorf("err = %v; want ErrIsolationUnavailable", err)
	}
}

func TestCheckIsolationAvailable_MissingNodeAndNilStateNoop(t *testing.T) {
	// Defensive: nil state or unknown hostname doesn't gate (those are
	// boot-time scenarios where the local Node hasn't registered yet).
	if err := lifecycle.CheckIsolationAvailable(nil, "node-a"); err != nil {
		t.Errorf("nil state should be a no-op; got %v", err)
	}
	if err := lifecycle.CheckIsolationAvailable(newStateWithNode("node-b", pb.NodeStatus_NODE_STATUS_READY), "ghost"); err != nil {
		t.Errorf("unknown hostname should be a no-op; got %v", err)
	}
	if err := lifecycle.CheckIsolationAvailable(newStateWithNode("node-a", pb.NodeStatus_NODE_STATUS_READY), ""); err != nil {
		t.Errorf("empty hostname should be a no-op; got %v", err)
	}
}

func TestCheckIsolationAvailable_OtherStatusesPass(t *testing.T) {
	// Only ISOLATION_UNAVAILABLE gates — JOINING, READY, even UNSPECIFIED
	// should pass.
	for _, st := range []pb.NodeStatus{
		pb.NodeStatus_NODE_STATUS_UNSPECIFIED,
		pb.NodeStatus_NODE_STATUS_JOINING,
		pb.NodeStatus_NODE_STATUS_READY,
	} {
		s := newStateWithNode("node-a", st)
		if err := lifecycle.CheckIsolationAvailable(s, "node-a"); err != nil {
			t.Errorf("status %v should pass; got %v", st, err)
		}
	}
}
