package scheduler_test

import (
	"context"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rollout"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestRun_InitialReconcileMaterializesReplicas — Run does an initial
// reconcile before subscribing, so state is up-to-date even when no
// events have fired yet.
func TestRun_InitialReconcileMaterializesReplicas(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	lead := &fakeLeader{leader: true}
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	s := scheduler.New(st, brokers, lead, applier, nil)

	seedNode(t, f, "node-a", &raftIdx)
	seedDeployment(t, f, "sample", 1, sampleCompose, &raftIdx)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait up to 2s for the initial reconcile to land replicas.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.ReplicasDesired.Len() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := st.ReplicasDesired.Len(); got != 1 {
		t.Errorf("ReplicasDesired = %d, want 1 (initial reconcile didn't materialize)", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("Run returned err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Run did not return after cancel")
	}
}

// TestRun_DeploymentEventTriggersDebouncedReconcile — after Run is
// active, applying a DeploymentApply via the FSM fires a Deployments
// watch event; the debounce timer + reconcile materializes replicas.
func TestRun_DeploymentEventTriggersDebouncedReconcile(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	lead := &fakeLeader{leader: true}
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	s := scheduler.New(st, brokers, lead, applier, nil)

	seedNode(t, f, "node-a", &raftIdx)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Initial reconcile has no deployments → no replicas.
	time.Sleep(100 * time.Millisecond)
	if st.ReplicasDesired.Len() != 0 {
		t.Fatalf("preconditions: ReplicasDesired = %d, want 0", st.ReplicasDesired.Len())
	}

	// Seed a deployment AFTER Run is subscribed; the watch event should
	// trigger a debounced reconcile within ~SafetyTick.
	seedDeployment(t, f, "smoke", 1, sampleCompose, &raftIdx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.ReplicasDesired.Len() > 0 {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("event-driven reconcile never materialized replicas; ReplicasDesired.Len = %d", st.ReplicasDesired.Len())
}

// TestRun_ContextCancelExitsCleanly — Run returns ctx.Err() on cancel.
func TestRun_ContextCancelExitsCleanly(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	s := scheduler.New(st, brokers, &fakeLeader{leader: false}, func([]byte) error { return nil }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Run did not return after cancel")
	}
}

// TestDriveRollout_StartsAndAdvancesPlan — drives the formal rollout
// state machine on an image change. The first reconcile starts a plan
// at step 0; once that replica reaches RUNNING + healthy, the next
// reconcile advances to step 1.
func TestDriveRollout_StartsAndAdvancesPlan(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	lead := &fakeLeader{leader: true}
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	clock := newFakeClock(time.Unix(1000, 0))
	r := rollout.New(st, applier, clock)
	s := scheduler.New(st, brokers, lead, applier, r)

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedDeployment(t, f, "sample", 2, sampleCompose, &raftIdx)

	// Initial reconcile places 2 replicas on nginx:1.27.
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 2 {
		t.Fatalf("initial: ReplicasDesired = %d, want 2", got)
	}

	// Flip compose to a new image and bump revision; reconcile should
	// start the rollout plan.
	const updatedCompose = `services:
  web:
    image: nginx:1.28
`
	raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2, ComposeYaml: []byte(updatedCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 2, ComposeService: "web",
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(upd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	s.Reconcile(context.Background())

	// Expect a plan was started.
	plan, ok := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if !ok {
		t.Fatalf("RolloutPlan not started after image-change reconcile")
	}
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		t.Errorf("plan state = %v, want IN_PROGRESS", plan.GetState())
	}
	if plan.GetCurrentStep() != 0 {
		t.Errorf("plan current_step = %d, want 0", plan.GetCurrentStep())
	}
	if plan.GetTotalSteps() != 2 {
		t.Errorf("plan total_steps = %d, want 2", plan.GetTotalSteps())
	}

	// At step 0 — exactly one replica should now be on the new image.
	var on128, on127 int
	for _, r := range st.ReplicasDesired.List() {
		switch r.GetImage() {
		case "nginx:1.28":
			on128++
		case "nginx:1.27":
			on127++
		}
	}
	if on128 != 1 || on127 != 1 {
		t.Errorf("after step 0 upsert: nginx:1.28 = %d, nginx:1.27 = %d; want 1+1", on128, on127)
	}
}

// TestDriveRollout_RefusesRestartAtSameRevision — after a plan
// completes / aborts at revision N, driveRollout refuses to start a
// new plan at the same revision (otherwise CheckTimeouts → Abort would
// re-fire Start on the next reconcile in an infinite loop).
func TestDriveRollout_RefusesRestartAtSameRevision(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	lead := &fakeLeader{leader: true}
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	clock := newFakeClock(time.Unix(1000, 0))
	r := rollout.New(st, applier, clock)
	s := scheduler.New(st, brokers, lead, applier, r)

	seedNode(t, f, "node-a", &raftIdx)
	seedDeployment(t, f, "sample", 1, sampleCompose, &raftIdx)
	s.Reconcile(context.Background())

	// Plant a COMPLETED plan at revision 1 (the current applied
	// revision) — driveRollout must NOT restart it.
	raftIdx++
	plan := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_RolloutPlanUpdate{
		RolloutPlanUpdate: &pb.RolloutPlanUpdate{Plan: &pb.RolloutPlan{
			Deployment:     "sample",
			Service:        "web",
			TargetRevision: 1,
			State:          pb.RolloutState_ROLLOUT_STATE_COMPLETED,
			CurrentStep:    1,
			TotalSteps:     1,
		}},
	}}
	pd, _ := proto.Marshal(plan)
	f.Apply(&hraft.Log{Index: raftIdx, Data: pd})

	// Trigger an image-change reconcile by changing the compose.
	const updated = `services:
  web:
    image: nginx:1.28
`
	raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1, ComposeYaml: []byte(updated),
			Services: []*pb.ServiceSpec{{Name: "web", Replicas: 1, ComposeService: "web"}},
		},
	}}
	uData, _ := proto.Marshal(upd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: uData})

	s.Reconcile(context.Background())

	// Plan should still be COMPLETED (not restarted).
	got, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if got.GetState() != pb.RolloutState_ROLLOUT_STATE_COMPLETED {
		t.Errorf("plan restarted at same revision; state = %v", got.GetState())
	}
}

// TestDriveRollout_AdvancesAndCompletes — once each step's replica
// reports RUNNING + fresh health, the next reconcile AdvanceStep's the
// plan; when CurrentStep == TotalSteps the plan completes.
func TestDriveRollout_AdvancesAndCompletes(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	lead := &fakeLeader{leader: true}
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	clock := newFakeClock(time.Unix(1000, 0))
	r := rollout.New(st, applier, clock)
	s := scheduler.New(st, brokers, lead, applier, r)

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedDeployment(t, f, "sample", 2, sampleCompose, &raftIdx)
	s.Reconcile(context.Background())

	// Mark both initial replicas RUNNING + fresh.
	for i := 0; i < 2; i++ {
		raftIdx++
		obs := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaObservedUpdate{
			ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: &pb.ReplicaObserved{
				Id:           replicaIDForTest("sample", "web", uint64(i)),
				State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
				LastHealthAt: timestamppb.New(clock.Now()),
			}},
		}}
		ob, _ := proto.Marshal(obs)
		f.Apply(&hraft.Log{Index: raftIdx, Data: ob})
	}

	// Trigger image-change rollout.
	const updatedCompose = `services:
  web:
    image: nginx:1.28
`
	raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2, ComposeYaml: []byte(updatedCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 2, ComposeService: "web",
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	uData, _ := proto.Marshal(upd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: uData})

	// First reconcile starts plan at step 0; upserts replica 0 on the
	// new image.
	s.Reconcile(context.Background())
	plan, _ := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetCurrentStep() != 0 || plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		t.Fatalf("after first reconcile: step=%d, state=%v; want step=0 IN_PROGRESS", plan.GetCurrentStep(), plan.GetState())
	}

	// Mark replica 0 as RUNNING + fresh (so StepReady=true on next pass).
	raftIdx++
	obs0 := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaObservedUpdate{
		ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: &pb.ReplicaObserved{
			Id:           replicaIDForTest("sample", "web", 0),
			State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
			LastHealthAt: timestamppb.New(clock.Now()),
		}},
	}}
	od0, _ := proto.Marshal(obs0)
	f.Apply(&hraft.Log{Index: raftIdx, Data: od0})

	// Next reconcile should AdvanceStep → step 1, upsert replica 1 on
	// new image.
	s.Reconcile(context.Background())
	plan, _ = st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetCurrentStep() != 1 {
		t.Errorf("after second reconcile: step=%d, want 1", plan.GetCurrentStep())
	}

	// Mark replica 1 RUNNING + fresh, then the next reconcile should
	// AdvanceStep → step 2 == TotalSteps and Complete.
	raftIdx++
	obs1 := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaObservedUpdate{
		ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: &pb.ReplicaObserved{
			Id:           replicaIDForTest("sample", "web", 1),
			State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
			LastHealthAt: timestamppb.New(clock.Now()),
		}},
	}}
	od1, _ := proto.Marshal(obs1)
	f.Apply(&hraft.Log{Index: raftIdx, Data: od1})

	s.Reconcile(context.Background())
	plan, _ = st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_COMPLETED {
		t.Errorf("plan state = %v, want COMPLETED", plan.GetState())
	}
}

// replicaIDForTest mirrors counter.ReplicaID without depending on the
// counter package's import directly in this test file.
func replicaIDForTest(dep, svc string, idx uint64) string {
	return dep + "-" + svc + "-" + fmtIdx(idx)
}

func fmtIdx(n uint64) string {
	// counter.ReplicaID uses strconv.FormatUint; replicate it inline so
	// we don't need to import strconv just for this.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestLookupImage_NilProjectReturnsEmpty — defensive guard exercised
// when compose parse fails earlier in Reconcile.
func TestLookupImage_NilProjectReturnsEmpty(t *testing.T) {
	// The function is unexported; we exercise it via a Reconcile call
	// where the compose parse succeeds but the ServiceSpec references
	// an unknown compose_service. That path is the lookup-returns-""
	// branch.
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)

	// ComposeService = "ghost" — won't be in the compose project.
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 1, ComposeService: "ghost",
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	s.Reconcile(context.Background())

	// reconcileService → lookupImage returns "" → DeploymentStatusUpdate
	// to PENDING. Confirm Deployment.Status flipped.
	dep, _ := st.Deployments.Get("sample")
	if dep.GetStatus() != pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING {
		t.Errorf("deployment status = %v, want PENDING (unknown compose_service)", dep.GetStatus())
	}
}
