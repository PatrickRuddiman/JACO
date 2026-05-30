package drain_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/drain"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func makeState() *state.State {
	return state.New(watch.NewRegistry())
}

func seedNode(st *state.State, name string) {
	st.Nodes.Apply(&pb.Node{Hostname: name, Status: pb.NodeStatus_NODE_STATUS_READY}, 1)
}

func seedReplica(st *state.State, dep, svc, host string, idx int32) {
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: dep + "-" + svc + "-" + itoa(idx),
		Deployment: dep, Service: svc, Index: idx, Host: host, Image: "img:1",
	}, 1)
}

func seedDeployment(st *state.State, name string, spec *pb.ServiceSpec) {
	st.Deployments.Apply(&pb.Deployment{Name: name, Services: []*pb.ServiceSpec{spec}}, 1)
}

func TestPlan_EmptyWhenHostHasNoReplicas(t *testing.T) {
	st := makeState()
	seedNode(st, "node-a")
	seedNode(st, "node-b")
	migs, err := drain.Plan(st, "node-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(migs) != 0 {
		t.Errorf("expected empty plan; got %+v", migs)
	}
}

func TestPlan_MigratesReplicasOffDrainingHost(t *testing.T) {
	st := makeState()
	for _, h := range []string{"node-a", "node-b", "node-c"} {
		seedNode(st, h)
	}
	seedDeployment(st, "sample", &pb.ServiceSpec{
		Name: "web", Replicas: 3, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
	})
	seedReplica(st, "sample", "web", "node-a", 0)
	seedReplica(st, "sample", "web", "node-b", 1)
	seedReplica(st, "sample", "web", "node-c", 2)

	migs, err := drain.Plan(st, "node-b")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("migrations = %d, want 1", len(migs))
	}
	if migs[0].FromHost != "node-b" {
		t.Errorf("FromHost = %q, want node-b", migs[0].FromHost)
	}
	if migs[0].ToHost == "node-b" {
		t.Errorf("ToHost = %q; must not be the draining host", migs[0].ToHost)
	}
	if migs[0].ToHost != "node-a" && migs[0].ToHost != "node-c" {
		t.Errorf("ToHost = %q; want node-a or node-c", migs[0].ToHost)
	}
}

func TestPlan_TwoReplicasOnSameHostBothMigrate(t *testing.T) {
	st := makeState()
	for _, h := range []string{"node-a", "node-b", "node-c"} {
		seedNode(st, h)
	}
	seedDeployment(st, "sample", &pb.ServiceSpec{
		Name: "web", Replicas: 3, Placement: pb.ServiceSpec_PLACEMENT_MODE_PACK,
	})
	// All 3 replicas on node-b (extreme pack scenario).
	seedReplica(st, "sample", "web", "node-b", 0)
	seedReplica(st, "sample", "web", "node-b", 1)
	seedReplica(st, "sample", "web", "node-b", 2)

	migs, err := drain.Plan(st, "node-b")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(migs) != 3 {
		t.Fatalf("migrations = %d, want 3", len(migs))
	}
	hosts := map[string]int{}
	for _, m := range migs {
		hosts[m.ToHost]++
	}
	if _, ok := hosts["node-b"]; ok {
		t.Errorf("a migration targets the draining host: %+v", migs)
	}
}

func TestPlan_FailsWhenNoEligibleRemains(t *testing.T) {
	// Service pinned to node-b; draining node-b leaves no eligible host.
	st := makeState()
	for _, h := range []string{"node-a", "node-b"} {
		seedNode(st, h)
	}
	seedDeployment(st, "sample", &pb.ServiceSpec{
		Name:      "web",
		Replicas:  1,
		Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
		Hosts:     []string{"node-b"},
	})
	seedReplica(st, "sample", "web", "node-b", 0)

	_, err := drain.Plan(st, "node-b")
	if err == nil {
		t.Fatalf("expected error; no eligible host remains")
	}
}

func TestPlan_RejectsEmptyHostname(t *testing.T) {
	_, err := drain.Plan(makeState(), "")
	if err == nil {
		t.Errorf("expected error on empty hostname")
	}
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestPlan_GlobalServiceReplicaDroppedNotMigrated guards the daemonset drain
// rule: a GLOBAL service runs one replica per node, so the draining host's
// replica must be DROPPED (no migration) — migrating it would double-place the
// daemonset on a node that already runs its own copy.
func TestPlan_GlobalServiceReplicaDroppedNotMigrated(t *testing.T) {
	f, st := newFSMState(t)
	var idx uint64
	seedNodeReady(t, f, "node-a", &idx)
	seedNodeReady(t, f, "node-b", &idx)
	seedDeploymentWithSvc(t, f, "sample", &pb.ServiceSpec{
		Name: "agent", Placement: pb.ServiceSpec_PLACEMENT_MODE_GLOBAL,
	}, &idx)
	// One global replica per node (host-keyed ids).
	seedDesired(t, f, "sample", "agent", "sample-agent-node-a", "node-a", &idx)
	seedDesired(t, f, "sample", "agent", "sample-agent-node-b", "node-b", &idx)

	migs, err := drain.Plan(st, "node-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(migs) != 0 {
		t.Fatalf("global replica must be dropped, not migrated; got %d migration(s): %+v", len(migs), migs)
	}
}
