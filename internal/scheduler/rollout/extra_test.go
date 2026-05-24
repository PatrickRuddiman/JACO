package rollout_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rollout"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestComplete_RefusesMissingPlan — surfaces an error when there's no
// plan to complete.
func TestComplete_RefusesMissingPlan(t *testing.T) {
	st := state.New(watch.NewRegistry())
	r := rollout.New(st, func([]byte) error { return nil }, nil)
	if err := r.Complete("ghost", "svc"); err == nil {
		t.Errorf("Complete on missing plan returned nil err")
	}
}

// TestAdvanceStep_RefusesMissingPlan — same as above.
func TestAdvanceStep_RefusesMissingPlan(t *testing.T) {
	st := state.New(watch.NewRegistry())
	r := rollout.New(st, func([]byte) error { return nil }, nil)
	if err := r.AdvanceStep("ghost", "svc"); err == nil {
		t.Errorf("AdvanceStep on missing plan returned nil err")
	}
}

// TestAdvanceStep_RefusesNonInProgressPlan — once a plan is COMPLETED
// or ABORTED, AdvanceStep refuses.
func TestAdvanceStep_RefusesNonInProgressPlan(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.RolloutPlans.Apply(&pb.RolloutPlan{
		Deployment: "d", Service: "s",
		State: pb.RolloutState_ROLLOUT_STATE_COMPLETED,
	}, 1)
	r := rollout.New(st, func([]byte) error { return nil }, nil)
	if err := r.AdvanceStep("d", "s"); err == nil {
		t.Errorf("AdvanceStep on COMPLETED plan returned nil err")
	}
}

// TestAbort_RefusesMissingPlan — defensive guard.
func TestAbort_RefusesMissingPlan(t *testing.T) {
	st := state.New(watch.NewRegistry())
	r := rollout.New(st, func([]byte) error { return nil }, nil)
	if err := r.Abort(context.Background(), "ghost", "svc", "reason"); err == nil {
		t.Errorf("Abort on missing plan returned nil err")
	}
}

// TestStart_RefusesActivePlan — once IN_PROGRESS, Start refuses
// (idempotency caller's responsibility — drive Start only when no
// plan exists or the prior one terminated).
func TestStart_RefusesActivePlan(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.RolloutPlans.Apply(&pb.RolloutPlan{
		Deployment: "d", Service: "s",
		State:    pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS,
		StartedAt: timestamppb.New(time.Now()),
	}, 1)
	r := rollout.New(st, func([]byte) error { return nil }, nil)
	if err := r.Start("d", "s", 2, 3); err == nil {
		t.Errorf("Start with active plan returned nil err")
	}
}

// TestCheckTimeouts_NoInProgressNoOp — empty rollout state.
func TestCheckTimeouts_NoInProgressNoOp(t *testing.T) {
	st := state.New(watch.NewRegistry())
	r := rollout.New(st, func([]byte) error { return nil }, nil)
	aborted, err := r.CheckTimeouts(context.Background())
	if err != nil {
		t.Errorf("CheckTimeouts: %v", err)
	}
	if len(aborted) != 0 {
		t.Errorf("aborted = %v, want empty", aborted)
	}
}
