package scheduler_test

import (
	"context"
	"testing"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestReconcile_RebalancerHostChangeSurvivesNextReconcile is the
// regression guard for the runtime-relocation bug surfaced by #137: the
// rebalancer writes Host=<dst> on a ReplicaDesired, and the very next
// scheduler reconcile pass MUST leave that host alone. The old behavior
// (re-run placement every pass, overwrite Host with the hash-picked
// default) raced the rebalancer and reduced moves to oscillation —
// audit logs reported a MOVE, but the container never relocated because
// the next reconcile reverted Host on the way back to the runtime.
func TestReconcile_RebalancerHostChangeSurvivesNextReconcile(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)

	// Initial reconcile places 3 replicas across 3 nodes via SPREAD.
	s.Reconcile(context.Background())
	if got := st.ReplicasDesired.Len(); got != 3 {
		t.Fatalf("preconditions: ReplicasDesired = %d, want 3", got)
	}
	pre := snapshotReplicas(st)

	// Pick one replica and simulate a rebalancer move: rewrite its
	// Host to a different eligible node via the FSM (same path the
	// rebalancer uses — ReplicaDesiredUpsert).
	var victimID, srcHost, dstHost string
	for id, r := range pre {
		victimID = id
		srcHost = r.host
		break
	}
	for _, h := range []string{"node-a", "node-b", "node-c"} {
		if h != srcHost {
			dstHost = h
			break
		}
	}
	raftIdx++
	move := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
			Replica: &pb.ReplicaDesired{
				Id: victimID, Deployment: "sample", Service: "web",
				Host: dstHost, Image: "nginx:1.27",
			},
		},
	}}
	data, _ := proto.Marshal(move)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	// Sanity: the move landed in state.
	r, _ := st.ReplicasDesired.Get(victimID)
	if r.GetHost() != dstHost {
		t.Fatalf("preconditions: after move, host = %q, want %q", r.GetHost(), dstHost)
	}

	// Reconcile again — MUST NOT revert the move. This is the
	// load-bearing assertion: without stickiness, the next reconcile
	// recomputes Host from placement.pickSpreadHost and (deterministically)
	// overwrites dstHost back to srcHost.
	s.Reconcile(context.Background())
	r, _ = st.ReplicasDesired.Get(victimID)
	if r.GetHost() != dstHost {
		t.Errorf("scheduler reverted rebalancer's move: host = %q, want sticky %q (src was %q)",
			r.GetHost(), dstHost, srcHost)
	}

	// Several more passes — still sticky.
	for i := 0; i < 5; i++ {
		s.Reconcile(context.Background())
	}
	r, _ = st.ReplicasDesired.Get(victimID)
	if r.GetHost() != dstHost {
		t.Errorf("after 5 more reconciles, host = %q, want %q (rebalancer moves must stick)",
			r.GetHost(), dstHost)
	}
}

// TestReconcile_StickyHostDroppedWhenNodeBecomesIneligible ensures
// stickiness yields to admissibility: when the host an existing replica
// lives on stops being eligible (drained / unready), the scheduler
// re-places that replica onto a still-eligible host.
func TestReconcile_StickyHostDroppedWhenNodeBecomesIneligible(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)
	seedDeployment(t, f, "sample", 3, sampleCompose, &raftIdx)
	s.Reconcile(context.Background())

	// Drain node-b — every replica currently on node-b MUST be
	// re-placed onto an eligible node (a or c).
	unreadyNode(t, f, "node-b", &raftIdx)
	s.Reconcile(context.Background())

	for _, r := range st.ReplicasDesired.List() {
		if r.GetHost() == "node-b" {
			t.Errorf("replica %s still on drained node-b after reconcile", r.GetId())
		}
		if r.GetHost() != "node-a" && r.GetHost() != "node-c" {
			t.Errorf("replica %s host = %q; must be node-a or node-c", r.GetId(), r.GetHost())
		}
	}
}

// TestReconcile_HostsModeReplacesReplicaWhenHostDroppedFromSpec verifies
// that stickiness defers to the HOSTS pin list: editing the spec to drop
// a previously-pinned host forces re-placement off that host even though
// the host is still in the cluster's eligible set.
func TestReconcile_HostsModeReplacesReplicaWhenHostDroppedFromSpec(t *testing.T) {
	s, st, f, _ := newScheduler(t, true)
	var raftIdx uint64
	seedNode(t, f, "node-a", &raftIdx)
	seedNode(t, f, "node-b", &raftIdx)
	seedNode(t, f, "node-c", &raftIdx)

	// HOSTS placement pinned to a + b, one replica each.
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 2,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
				Hosts:     []string{"node-a", "node-b"},
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})
	s.Reconcile(context.Background())

	// Drop node-b from the pin list, add node-c instead.
	raftIdx++
	cmd2 := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2, ComposeYaml: []byte(sampleCompose),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 2,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
				Hosts:     []string{"node-a", "node-c"},
			}},
		},
	}}
	data2, _ := proto.Marshal(cmd2)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data2})
	s.Reconcile(context.Background())

	for _, r := range st.ReplicasDesired.List() {
		if r.GetHost() == "node-b" {
			t.Errorf("replica %s still on node-b after it was dropped from HOSTS spec", r.GetId())
		}
	}
}
