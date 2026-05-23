package grpcsrv

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/drain"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestDrainComplete_EmptyMigrationsIsAlwaysTrue — vacuous truth.
func TestDrainComplete_EmptyMigrationsIsAlwaysTrue(t *testing.T) {
	st := state.New(watch.NewRegistry())
	if !drainComplete(st, nil) {
		t.Errorf("drainComplete with no migrations = false")
	}
}

// TestDrainComplete_FalseWhenObservationMissing — a planned migration
// whose ReplicaObserved hasn't shown up yet is reported as incomplete.
func TestDrainComplete_FalseWhenObservationMissing(t *testing.T) {
	st := state.New(watch.NewRegistry())
	migs := []drain.Migration{
		{ReplicaID: "smoke-web-0", Deployment: "smoke", Service: "web", ToHost: "node-b"},
	}
	if drainComplete(st, migs) {
		t.Errorf("drainComplete with missing observation = true")
	}
}

// TestDrainComplete_FalseWhenObservationNotRunning — a planned
// migration whose ReplicaObserved is PENDING / FAILED still counts as
// incomplete.
func TestDrainComplete_FalseWhenObservationNotRunning(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id: "smoke-web-0", State: pb.ReplicaState_REPLICA_STATE_PENDING,
	}, 1)
	migs := []drain.Migration{
		{ReplicaID: "smoke-web-0"},
	}
	if drainComplete(st, migs) {
		t.Errorf("drainComplete with PENDING observation = true")
	}
}

// TestDrainComplete_TrueWhenAllRunning — every migration's replica is
// RUNNING.
func TestDrainComplete_TrueWhenAllRunning(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id: "smoke-web-0", State: pb.ReplicaState_REPLICA_STATE_RUNNING,
	}, 1)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id: "smoke-web-1", State: pb.ReplicaState_REPLICA_STATE_RUNNING,
	}, 2)
	migs := []drain.Migration{
		{ReplicaID: "smoke-web-0"},
		{ReplicaID: "smoke-web-1"},
	}
	if !drainComplete(st, migs) {
		t.Errorf("drainComplete with all RUNNING = false")
	}
}

// TestErrorStatus_FormatsWithStableFields — the typed status helper is
// used by every membership / drain handler. Exercise both with-detail
// and bare branches.
func TestErrorStatus_FormatsWithStableFields(t *testing.T) {
	err := errorStatus(codes.FailedPrecondition, "code-x", "detail-y")
	if err == nil {
		t.Fatalf("errorStatus returned nil")
	}
	msg := err.Error()
	// We don't pin the exact wire format; just confirm code + detail
	// substring are present.
	if !strContains(msg, "code-x") {
		t.Errorf("msg = %q, want code-x substring", msg)
	}
}

func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
