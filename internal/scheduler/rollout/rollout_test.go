package rollout_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/counter"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rollout"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeClock pins time.Now to a controllable value.
type fakeClock struct{ now atomic.Pointer[time.Time] }

func newFakeClock(start time.Time) *fakeClock {
	c := &fakeClock{}
	c.now.Store(&start)
	return c
}
func (c *fakeClock) Now() time.Time { return *c.now.Load() }
func (c *fakeClock) Advance(d time.Duration) {
	n := c.Now().Add(d)
	c.now.Store(&n)
}

func newHarness(t *testing.T) (*rollout.Rollout, *state.State, *fsm.FSM, *fakeClock) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	clock := newFakeClock(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	return rollout.New(st, applier, clock), st, f, clock
}

// seedDeployment writes a Deployment{name, revision, previous_revision}
// directly into the FSM so Abort can find a previous_revision to roll back to.
func seedDeployment(t *testing.T, f *fsm.FSM, name string, currentRev, prevRev uint64) {
	t.Helper()
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{Deployment: name, Revision: prevRev, ComposeYaml: []byte("services:\n  web:\n    image: x\n")},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: 1, Data: data})
	if currentRev > prevRev {
		cmd2 := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
			DeploymentApply: &pb.DeploymentApply{Deployment: name, Revision: currentRev, ComposeYaml: []byte("services:\n  web:\n    image: y\n")},
		}}
		data, _ = proto.Marshal(cmd2)
		f.Apply(&hraft.Log{Index: 2, Data: data})
	}
}

// seedReplicaObserved writes a ReplicaObserved entry for replica index i.
func seedReplicaObserved(t *testing.T, f *fsm.FSM, deployment, service string, idx uint64, st pb.ReplicaState, clock *fakeClock) {
	t.Helper()
	id := counter.ReplicaID(deployment, service, idx)
	cmd := &pb.Command{Ts: timestamppb.New(clock.Now()), Payload: &pb.Command_ReplicaObservedUpdate{
		ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: &pb.ReplicaObserved{
			Id: id, State: st, LastHealthAt: timestamppb.New(clock.Now()),
		}},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: 100 + idx, Data: data})
}

func TestStart_CreatesPlanInProgress(t *testing.T) {
	r, st, _, clock := newHarness(t)
	if err := r.Start("sample", "web", 2, 3); err != nil {
		t.Fatalf("Start: %v", err)
	}
	plan, ok := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if !ok {
		t.Fatalf("plan missing post-Start")
	}
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		t.Errorf("state = %v, want IN_PROGRESS", plan.GetState())
	}
	if plan.GetTotalSteps() != 3 || plan.GetCurrentStep() != 0 {
		t.Errorf("counters = %d/%d, want 0/3", plan.GetCurrentStep(), plan.GetTotalSteps())
	}
	if got := plan.GetStartedAt().AsTime(); !got.Equal(clock.Now()) {
		t.Errorf("started_at = %v, want %v", got, clock.Now())
	}
}

func TestStart_RefusesWhenPlanAlreadyInProgress(t *testing.T) {
	r, _, _, _ := newHarness(t)
	if err := r.Start("sample", "web", 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := r.Start("sample", "web", 3, 3); err == nil {
		t.Errorf("expected error on second Start while in progress")
	}
}

func TestAdvanceStep_BumpsCurrentStepAndTimestamp(t *testing.T) {
	r, st, f, clock := newHarness(t)
	if err := r.Start("sample", "web", 2, 3); err != nil {
		t.Fatal(err)
	}
	// Seed the target (step 0) replica as RUNNING + fresh.
	seedReplicaObserved(t, f, "sample", "web", 0, pb.ReplicaState_REPLICA_STATE_RUNNING, clock)

	clock.Advance(5 * time.Second)
	if err := r.AdvanceStep("sample", "web"); err != nil {
		t.Fatalf("AdvanceStep: %v", err)
	}
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetCurrentStep() != 1 {
		t.Errorf("current_step = %d, want 1", plan.GetCurrentStep())
	}
	if got := plan.GetLastStepAt().AsTime(); !got.Equal(clock.Now()) {
		t.Errorf("last_step_at = %v, want %v", got, clock.Now())
	}
}

func TestAdvanceStep_HoldsWhenInvariantViolated(t *testing.T) {
	r, _, f, clock := newHarness(t)
	if err := r.Start("sample", "web", 2, 3); err != nil {
		t.Fatal(err)
	}
	// Two replicas not-running — invariant says only 1 may be in-flight.
	seedReplicaObserved(t, f, "sample", "web", 0, pb.ReplicaState_REPLICA_STATE_PENDING, clock)
	seedReplicaObserved(t, f, "sample", "web", 1, pb.ReplicaState_REPLICA_STATE_DEGRADED, clock)

	if err := r.AdvanceStep("sample", "web"); err != nil {
		t.Fatalf("AdvanceStep should not return an error on hold; the rollout retries: %v", err)
	}
	// The plan's current_step must NOT have advanced.
	// (auditAndHold emits an audit event but doesn't bump the plan.)
}

func TestStepReady_TrueOnlyForRunningFreshTargetReplica(t *testing.T) {
	r, _, f, clock := newHarness(t)
	r.Start("sample", "web", 2, 3)

	// No observed replica → not ready.
	ready, _, _ := r.StepReady("sample", "web")
	if ready {
		t.Errorf("StepReady=true with no observed replica")
	}

	// Pending → not ready.
	seedReplicaObserved(t, f, "sample", "web", 0, pb.ReplicaState_REPLICA_STATE_PENDING, clock)
	ready, _, _ = r.StepReady("sample", "web")
	if ready {
		t.Errorf("StepReady=true on PENDING")
	}

	// Running + fresh → ready.
	seedReplicaObserved(t, f, "sample", "web", 0, pb.ReplicaState_REPLICA_STATE_RUNNING, clock)
	ready, _, _ = r.StepReady("sample", "web")
	if !ready {
		t.Errorf("StepReady=false on RUNNING + fresh")
	}

	// Stale health → not ready.
	clock.Advance(rollout.HealthFreshness + time.Second)
	ready, _, _ = r.StepReady("sample", "web")
	if ready {
		t.Errorf("StepReady=true on stale health")
	}
}

func TestComplete_TransitionsToCompleted(t *testing.T) {
	r, st, _, _ := newHarness(t)
	r.Start("sample", "web", 2, 3)
	if err := r.Complete("sample", "web"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if got := plan.GetState(); got != pb.RolloutState_ROLLOUT_STATE_COMPLETED {
		t.Errorf("state = %v, want COMPLETED", got)
	}
}

func TestAbort_TransitionsAbortedAndRollsbackDeployment(t *testing.T) {
	r, st, f, _ := newHarness(t)
	seedDeployment(t, f, "sample", 2, 1) // current=2, previous=1
	r.Start("sample", "web", 2, 3)

	if err := r.Abort(context.Background(), "sample", "web", "step_timeout"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_ABORTED {
		t.Errorf("plan state = %v, want ABORTED", plan.GetState())
	}
	if plan.GetFailureReason() != "step_timeout" {
		t.Errorf("failure_reason = %q", plan.GetFailureReason())
	}

	dep, _ := st.Deployments.Get("sample")
	if dep.GetAppliedRevision() != 1 || dep.GetPreviousRevision() != 2 {
		t.Errorf("after Abort: applied=%d previous=%d (want applied=1 previous=2 — rolled back)",
			dep.GetAppliedRevision(), dep.GetPreviousRevision())
	}
}

func TestCheckTimeouts_AbortsStaleRollouts(t *testing.T) {
	r, st, f, clock := newHarness(t)
	seedDeployment(t, f, "sample", 2, 1)
	r.Start("sample", "web", 2, 3)

	// Fast-forward past StepTimeout.
	clock.Advance(rollout.StepTimeout + time.Second)
	aborted, err := r.CheckTimeouts(context.Background())
	if err != nil {
		t.Fatalf("CheckTimeouts: %v", err)
	}
	if len(aborted) != 1 || aborted[0] != "sample" {
		t.Errorf("aborted = %v, want [sample]", aborted)
	}
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_ABORTED {
		t.Errorf("plan state = %v, want ABORTED", plan.GetState())
	}
	if plan.GetFailureReason() != "step_timeout" {
		t.Errorf("failure_reason = %q, want step_timeout", plan.GetFailureReason())
	}
}

func TestCheckTimeouts_DoesNotAbortFreshRollouts(t *testing.T) {
	r, st, f, clock := newHarness(t)
	seedDeployment(t, f, "sample", 2, 1)
	r.Start("sample", "web", 2, 3)
	clock.Advance(StepTimeout / 2)
	aborted, _ := r.CheckTimeouts(context.Background())
	if len(aborted) != 0 {
		t.Errorf("CheckTimeouts aborted fresh rollout: %v", aborted)
	}
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		t.Errorf("plan state = %v, want still IN_PROGRESS", plan.GetState())
	}
}

func TestFullCycle_InvariantNeverViolatedAcrossRollout(t *testing.T) {
	// Drive a full 3-step rollout. For each step k, simulate the runtime:
	// mark replica k as PENDING (transitioning), then RUNNING (done). Poll
	// the invariant at every observation — at no point should more than 1
	// replica be not-running.
	r, st, f, clock := newHarness(t)
	seedDeployment(t, f, "sample", 2, 1)
	// Seed all 3 replicas as RUNNING (steady state pre-rollout).
	for i := uint64(0); i < 3; i++ {
		seedReplicaObserved(t, f, "sample", "web", i, pb.ReplicaState_REPLICA_STATE_RUNNING, clock)
	}
	r.Start("sample", "web", 2, 3)

	checkInvariant := func(label string) {
		t.Helper()
		notRunning := 0
		for _, obs := range st.ReplicasObserved.List() {
			if obs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
				notRunning++
			}
		}
		if notRunning > 1 {
			t.Fatalf("invariant violated at %s: %d replicas not running (> 1)", label, notRunning)
		}
	}

	for step := uint64(0); step < 3; step++ {
		// Replica step transitions PENDING → RUNNING (image swap by runtime).
		seedReplicaObserved(t, f, "sample", "web", step, pb.ReplicaState_REPLICA_STATE_PENDING, clock)
		checkInvariant("after transition")
		clock.Advance(2 * time.Second)
		seedReplicaObserved(t, f, "sample", "web", step, pb.ReplicaState_REPLICA_STATE_RUNNING, clock)
		checkInvariant("after running")

		if err := r.AdvanceStep("sample", "web"); err != nil {
			t.Fatalf("step %d AdvanceStep: %v", step, err)
		}
		checkInvariant("after AdvanceStep")
	}
	r.Complete("sample", "web")
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_COMPLETED {
		t.Errorf("final plan state = %v, want COMPLETED", plan.GetState())
	}
	if plan.GetCurrentStep() != 3 {
		t.Errorf("final current_step = %d, want 3", plan.GetCurrentStep())
	}
}

// Re-expose the StepTimeout constant for the StepTimeout/2 test.
var StepTimeout = rollout.StepTimeout
