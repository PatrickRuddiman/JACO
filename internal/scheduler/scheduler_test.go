package scheduler_test

import (
	"context"
	"testing"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

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
	s := scheduler.New(st, brokers, lead, applier)
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
				Name: "web", Replicas: replicas, ComposeService: "web",
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
				Name: "web", Replicas: 1, ComposeService: "web",
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
	s := scheduler.New(st, brokers, &fakeLeader{leader: true}, applier)

	seedNode(t, f, "node-a", &raftIdx)
	// Apply a deployment pinning to node-z (which doesn't exist).
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "pinned", Revision: 1, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 1, ComposeService: "web",
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
	// Deployment references compose_service "ghost" which isn't in compose.
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
