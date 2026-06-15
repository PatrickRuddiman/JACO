package scheduler_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

// seedDeploymentGlobal writes a Deployment whose single "web" service uses
// PLACEMENT_MODE_GLOBAL (daemonset). replicas is set on the spec to prove the
// scheduler ignores it under global placement.
func seedDeploymentGlobal(t *testing.T, f *fsm.FSM, name string, replicas int32, composeYAML string, raftIdx *uint64) {
	t.Helper()
	*raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: name, Revision: 1, ComposeYaml: []byte(composeYAML),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: replicas,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_GLOBAL,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: *raftIdx, Data: data})
}

// unreadyNode flips a node out of NODE_STATUS_READY so EligibleHosts drops it
// (simulating a node leaving/draining). Any non-READY status works; JOINING is
// the status the FSM itself uses pre-promotion, so it's guaranteed to exist.
func unreadyNode(t *testing.T, f *fsm.FSM, name string, raftIdx *uint64) {
	t.Helper()
	*raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: name, Status: pb.NodeStatus_NODE_STATUS_JOINING,
		},
	}}
	data, _ := proto.Marshal(upd)
	f.Apply(&hraft.Log{Index: *raftIdx, Data: data})
}

func TestReconcile_GlobalPlacementOneReplicaPerReadyNode(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	// replicas:99 must be ignored under global.
	seedDeploymentGlobal(t, f, "sample", 99, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())

	replicas := st.ReplicasDesired.List()
	if got := len(replicas); got != 3 {
		t.Fatalf("global ReplicasDesired count = %d, want 3 (one per ready node)", got)
	}
	hosts := map[string]int{}
	for _, r := range replicas {
		hosts[r.GetHost()]++
	}
	if len(hosts) != 3 {
		t.Errorf("hosts used = %d (%v); want 3 distinct hosts", len(hosts), hosts)
	}
	for h, c := range hosts {
		if c != 1 {
			t.Errorf("host %s got %d replicas; want exactly 1 under global", h, c)
		}
	}
}

func TestReconcile_GlobalPlacementGrowsOnNodeJoin(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedDeploymentGlobal(t, f, "sample", 0, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 2 {
		t.Fatalf("before join: ReplicasDesired = %d, want 2", got)
	}

	// New node joins → count must climb to 3.
	seedNode(t, f, "node-c", &raftIdx)
	s.Reconcile(context.Background())

	replicas := st.ReplicasDesired.List()
	if got := len(replicas); got != 3 {
		t.Fatalf("after join: ReplicasDesired = %d, want 3", got)
	}
	hosts := map[string]bool{}
	for _, r := range replicas {
		hosts[r.GetHost()] = true
	}
	for _, want := range []string{"node-a", "node-b", "node-c"} {
		if !hosts[want] {
			t.Errorf("after join: missing replica on %s (hosts=%v)", want, hosts)
		}
	}
}

func TestReconcile_GlobalPlacementShrinksOnNodeLeave(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeploymentGlobal(t, f, "sample", 0, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 3 {
		t.Fatalf("before leave: ReplicasDesired = %d, want 3", got)
	}

	// node-b leaves (no longer READY) → its replica must be removed.
	unreadyNode(t, f, "node-b", &raftIdx)
	s.Reconcile(context.Background())

	replicas := st.ReplicasDesired.List()
	if got := len(replicas); got != 2 {
		t.Fatalf("after leave: ReplicasDesired = %d, want 2", got)
	}
	for _, r := range replicas {
		if r.GetHost() == "node-b" {
			t.Errorf("after leave: replica %s still pinned to drained node-b", r.GetId())
		}
	}
}

// TestReconcile_GlobalPlacementSurvivorIDsStableOnNodeLeave guards the
// host-keyed replica id: when one node leaves, the surviving nodes' replicas
// must keep their exact ids so their containers are not torn down and
// recreated. A position-keyed id would re-index survivors and churn every
// container for one unrelated departure.
func TestReconcile_GlobalPlacementSurvivorIDsStableOnNodeLeave(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeploymentGlobal(t, f, "sample", 0, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())
	before := map[string]string{} // host -> replica id
	for _, r := range st.ReplicasDesired.List() {
		before[r.GetHost()] = r.GetId()
	}
	if before["node-a"] == "" || before["node-c"] == "" {
		t.Fatalf("expected replicas on node-a and node-c, got %v", before)
	}

	// node-b (lexically in the middle) leaves.
	raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: "node-b", Status: pb.NodeStatus_NODE_STATUS_JOINING,
		},
	}}
	data, _ := proto.Marshal(upd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})
	s.Reconcile(context.Background())

	after := map[string]string{}
	for _, r := range st.ReplicasDesired.List() {
		after[r.GetHost()] = r.GetId()
	}
	if len(after) != 2 {
		t.Fatalf("want 2 replicas after node-b leaves, got %d (%v)", len(after), after)
	}
	if after["node-a"] != before["node-a"] {
		t.Errorf("node-a replica id churned: %q -> %q", before["node-a"], after["node-a"])
	}
	if after["node-c"] != before["node-c"] {
		t.Errorf("node-c replica id churned: %q -> %q", before["node-c"], after["node-c"])
	}
}

func TestReconcile_GlobalPlacementIgnoresReplicasField(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64

	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	// replicas:1 set, but global must still place one per node → 2.
	seedDeploymentGlobal(t, f, "sample", 1, sampleCompose, &raftIdx)

	s.Reconcile(context.Background())

	if got := st.ReplicasDesired.Len(); got != 2 {
		t.Fatalf("global with replicas:1 → ReplicasDesired = %d, want 2 (count == nodes)", got)
	}
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
	rollouts := rollout.New(st, applier, clock.Now)
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
	var logBuf bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

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
	if logs := logBuf.String(); !strings.Contains(logs, "deployment scheduling blocked") || !strings.Contains(logs, "pinned") {
		t.Errorf("scheduler did not log the scheduling-blocked transition; got:\n%s", logs)
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

// TestReconcile_EnvValueChangeForcesUpsert pins the fix for issue #148:
// when the resolved compose YAML changes a service's env VALUE (image and
// host stay the same), the scheduler MUST emit a ReplicaDesiredUpsert so
// the FSM bumps RaftIndex and the runtime reconciler recreates the
// container with the new env baked in. Pre-fix the upsert gate only
// compared (Host, Image) and silently skipped — leaving the container
// pinned to its first env values across every subsequent apply.
func TestReconcile_EnvValueChangeForcesUpsert(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64

	seedNode(t, f, "node-a", &raftIdx)
	before := `services:
  web:
    image: nginx:1.27
    environment:
      DB_PASS: hunter2
`
	seedDeployment(t, f, "sample", 1, before, &raftIdx)
	s.Reconcile(context.Background())

	pre := st.ReplicasDesired.List()
	if len(pre) != 1 {
		t.Fatalf("after seed: ReplicasDesired = %d, want 1", len(pre))
	}
	preIdx := pre[0].GetRaftIndex()
	preHash := pre[0].GetSpecHash()
	if len(preHash) == 0 {
		t.Errorf("spec_hash should be populated on the initial upsert")
	}

	// Re-apply with the SAME compose: scheduler should NOT bump RaftIndex
	// (no drift). Pre-fix this was the only path that wouldn't churn.
	seedDeployment(t, f, "sample", 1, before, &raftIdx)
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.List()[0].GetRaftIndex(); got != preIdx {
		t.Errorf("idempotent re-apply bumped RaftIndex from %d to %d", preIdx, got)
	}

	// Change the env VALUE only. Image, host, services unchanged. Pre-fix:
	// scheduler short-circuited at the (Host, Image) gate, no upsert
	// emitted, container stays pinned to hunter2 forever. Post-fix: hash
	// flips, upsert fires, RaftIndex bumps.
	after := `services:
  web:
    image: nginx:1.27
    environment:
      DB_PASS: hunter3
`
	seedDeployment(t, f, "sample", 1, after, &raftIdx)
	s.Reconcile(context.Background())

	post := st.ReplicasDesired.List()
	if len(post) != 1 {
		t.Fatalf("after env edit: ReplicasDesired = %d, want 1", len(post))
	}
	if post[0].GetRaftIndex() <= preIdx {
		t.Errorf("env-value change did not bump RaftIndex: pre=%d post=%d (drift went undetected — #148 regression)",
			preIdx, post[0].GetRaftIndex())
	}
	if bytes.Equal(post[0].GetSpecHash(), preHash) {
		t.Errorf("env-value change did not flip spec_hash: hash=%x", preHash)
	}
}
