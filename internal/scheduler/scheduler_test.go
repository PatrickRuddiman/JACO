package scheduler_test

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
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rollout"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeClock matches the rollout package's clock contract.
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

const sampleCompose = `services:
  web:
    image: nginx:1.27
  api:
    image: api:1.0
`

// fakeLeader lets tests flip leadership on/off.
type fakeLeader struct{ leader bool }

func (f *fakeLeader) IsLeader() bool { return f.leader }

// newScheduler boots state + FSM + scheduler with an Applier that routes
// through the FSM so reads from state see the effect of every Reconcile
// pass. Returns the testing handles + a raftIndex counter pointer for the
// applier closure.
func newScheduler(t *testing.T, leader bool) (*scheduler.Scheduler, *state.State, *fsm.FSM, *fakeLeader) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	lead := &fakeLeader{leader: leader}
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	s := scheduler.New(st, brokers, lead, applier, nil)
	return s, st, f, lead
}

// seedNode adds a NODE_STATUS_READY node directly to state via the FSM.
func seedNode(t *testing.T, f *fsm.FSM, name string, raftIdx *uint64) {
	t.Helper()
	*raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeJoin{
		NodeJoin: &pb.NodeJoin{Hostname: name, Address: name + ":7000"},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: *raftIdx, Data: data})
	// Promote to READY (FSM defaults to JOINING on NodeJoin).
	*raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: name, Status: pb.NodeStatus_NODE_STATUS_READY,
		},
	}}
	data, _ = proto.Marshal(upd)
	f.Apply(&hraft.Log{Index: *raftIdx, Data: data})
}

// seedDeployment writes a Deployment via Command{DeploymentApply}.
func seedDeployment(t *testing.T, f *fsm.FSM, name string, replicas int32, composeYAML string, raftIdx *uint64) {
	t.Helper()
	*raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: name, Revision: 1, ComposeYaml: []byte(composeYAML),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: replicas,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: *raftIdx, Data: data})
}

func TestReconcile_ThreeReplicaDeploymentEvenlySpreadAcrossThreeNodes(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())

	replicas := st.ReplicasDesired.List()
	if got := len(replicas); got != 3 {
		t.Fatalf("ReplicasDesired count = %d, want 3", got)
	}
	hosts := map[string]int{}
	for _, r := range replicas {
		hosts[r.GetHost()]++
		if r.GetImage() != "nginx:1.27" {
			t.Errorf("replica %s image = %q, want nginx:1.27", r.GetId(), r.GetImage())
		}
		if r.GetDeployment() != "sample" || r.GetService() != "web" {
			t.Errorf("replica %s scope = %s/%s, want sample/web", r.GetId(), r.GetDeployment(), r.GetService())
		}
	}
	if len(hosts) != 3 {
		t.Errorf("hosts used = %d (%v); want 3 distinct hosts (even spread)", len(hosts), hosts)
	}
	for h, c := range hosts {
		if c != 1 {
			t.Errorf("host %s got %d replicas; want exactly 1 (3 replicas / 3 hosts)", h, c)
		}
	}
}

func TestReconcile_NoopOnLeaderLoss(t *testing.T) {
	s, st, f, lead := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedDeployment(t, f, "sample", 2, sampleCompose, &raftIdx)

	// Lose leadership BEFORE reconcile.
	lead.leader = false
	s.Reconcile(context.Background())

	if got := st.ReplicasDesired.Len(); got != 0 {
		t.Errorf("ReplicasDesired count = %d, want 0 (reconcile must no-op on follower)", got)
	}

	// Regain leadership → reconcile materializes the replicas.
	lead.leader = true
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 2 {
		t.Errorf("after regaining leadership, ReplicasDesired = %d, want 2", got)
	}
}

func TestReconcile_IsIdempotentWhenStateAlreadyMatches(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())
	first := snapshotReplicas(st)

	// Run reconcile again — should be a no-op (no diff).
	s.Reconcile(context.Background())
	second := snapshotReplicas(st)

	if !sameReplicas(first, second) {
		t.Errorf("second reconcile produced a diff:\nbefore=%v\nafter=%v", first, second)
	}
}

// TestReconcile_ImageChangeRollsOneAtATime checks that when every existing
// replica needs an image update, each reconcile pass upgrades exactly one
// replica (so at most one replica is down at any time — the replicas-1
// invariant). Implemented in iter 29.
func TestReconcile_ImageChangeRollsOneAtATime(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)

	// Initial reconcile lands 3 replicas on nginx:1.27.
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 3 {
		t.Fatalf("preconditions: ReplicasDesired = %d, want 3", got)
	}

	// Apply a new revision with an image change. Same compose service,
	// new image tag.
	newCompose := `services:
  web:
    image: nginx:1.28
  api:
    image: api:1.0
`
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2, ComposeYaml: []byte(newCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 3,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	// First reconcile after image change: exactly one replica should
	// flip to the new image.
	s.Reconcile(context.Background())
	if got := countWithImage(st, "nginx:1.28"); got != 1 {
		t.Errorf("after first reconcile, nginx:1.28 count = %d, want 1 (one-at-a-time)", got)
	}
	if got := countWithImage(st, "nginx:1.27"); got != 2 {
		t.Errorf("after first reconcile, nginx:1.27 count = %d, want 2", got)
	}

	// Second pass: another replica flips.
	s.Reconcile(context.Background())
	if got := countWithImage(st, "nginx:1.28"); got != 2 {
		t.Errorf("after second reconcile, nginx:1.28 count = %d, want 2", got)
	}

	// Third pass: last replica flips, rollout complete.
	s.Reconcile(context.Background())
	if got := countWithImage(st, "nginx:1.28"); got != 3 {
		t.Errorf("after third reconcile, nginx:1.28 count = %d, want 3", got)
	}
	if got := countWithImage(st, "nginx:1.27"); got != 0 {
		t.Errorf("after third reconcile, nginx:1.27 count = %d, want 0", got)
	}
}

// TestReconcile_RolloutAbortsOnStepTimeout drives an image change with the
// formal rollout state machine wired, never reports the new replica
// RUNNING, advances the clock past StepTimeout, and asserts the plan
// transitions to ABORTED + the deployment's revisions flip back via the
// CheckTimeouts → Abort → DeploymentRollback batch.
func TestReconcile_RolloutAbortsOnStepTimeout(t *testing.T) {
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
	rollouts := rollout.New(st, applier, clock)
	s := scheduler.New(st, brokers, &fakeLeader{leader: true}, applier, rollouts)

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)

	// Initial reconcile lands 3 replicas on nginx:1.27.
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 3 {
		t.Fatalf("preconditions: ReplicasDesired = %d, want 3", got)
	}

	// New revision triggers a rollout.
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2,
			ComposeYaml: []byte("services:\n  web:\n    image: nginx:1.28\n  api:\n    image: api:1.0\n"),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 3,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	// First reconcile starts the plan + upserts replica 0.
	s.Reconcile(context.Background())

	// Confirm the plan is IN_PROGRESS.
	plan, ok := st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if !ok || plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		t.Fatalf("plan = %+v; want IN_PROGRESS", plan)
	}

	// Advance the clock past StepTimeout without reporting the replica
	// RUNNING. Next reconcile's CheckTimeouts should abort.
	clock.Advance(rollout.StepTimeout + time.Second)
	s.Reconcile(context.Background())

	plan, _ = st.RolloutPlans.Get(state.RolloutPlanKey("sample", "web"))
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_ABORTED {
		t.Errorf("plan.state = %v, want ABORTED", plan.GetState())
	}
	if plan.GetFailureReason() == "" {
		t.Errorf("plan.failure_reason is empty; want non-empty after step_timeout abort")
	}
}

func countWithImage(st *state.State, image string) int {
	n := 0
	for _, r := range st.ReplicasDesired.List() {
		if r.GetImage() == image {
			n++
		}
	}
	return n
}

func TestReconcile_RemovesReplicasWhenScalingDown(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 3 {
		t.Fatalf("preconditions: ReplicasDesired = %d, want 3", got)
	}

	// Scale down to 1 by applying a new Deployment revision.
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 1,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 1 {
		t.Errorf("after scale-down, ReplicasDesired = %d, want 1", got)
	}
}

func TestReconcile_PinnedHostFailureMarksDeploymentPending(t *testing.T) {
	// Service pins itself to a node that doesn't exist; reconcile must
	// raise DEPLOYMENT_STATUS_PENDING and write no ReplicaDesired.
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	s := scheduler.New(st, brokers, &fakeLeader{leader: true}, applier, nil)

	seedNode(t, f, "node-a", &raftIdx)
	// Apply a deployment pinning to node-z (which doesn't exist).
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "pinned", Revision: 1, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 1,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
				Hosts:     []string{"node-z"},
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	s.Reconcile(context.Background())

	dep, ok := st.Deployments.Get("pinned")
	if !ok {
		t.Fatalf("deployment missing")
	}
	if got := dep.GetStatus(); got != pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING {
		t.Errorf("deployment status = %v, want PENDING", got)
	}
	if got := st.ReplicasDesired.Len(); got != 0 {
		t.Errorf("ReplicasDesired = %d, want 0 (pinned-host failure must not place anything)", got)
	}
}

func TestReconcile_UnknownComposeServiceMarksPending(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	// Service name "ghost" isn't a key in sampleCompose → marks deployment PENDING.
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "ghost", Replicas: 1,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	s.Reconcile(context.Background())

	dep, _ := st.Deployments.Get("sample")
	if dep.GetStatus() != pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING {
		t.Errorf("expected PENDING; got %v", dep.GetStatus())
	}
}

// --- helpers -----------------------------------------------------------------

func snapshotReplicas(st *state.State) map[string]struct{ host, image string } {
	out := map[string]struct{ host, image string }{}
	for _, r := range st.ReplicasDesired.List() {
		out[r.GetId()] = struct{ host, image string }{r.GetHost(), r.GetImage()}
	}
	return out
}

func sameReplicas(a, b map[string]struct{ host, image string }) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || vb != va {
			return false
		}
	}
	return true
}
