package rebalance_test

import (
	"context"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestQuorum_HonorsFloor — for various (declared N, currently
// running count) combinations, WouldBreakQuorum returns true exactly
// when post-stop count drops below ⌈N/2⌉ + 1.
func TestQuorum_HonorsFloor(t *testing.T) {
	cases := []struct {
		name           string
		n              int // declared replicas
		runningMembers int // currently RUNNING same-service replicas
		wantBreaks     bool
	}{
		{"N=1 has no quorum constraint", 1, 1, false},
		{"N=3 with all 3 running, stopping one leaves 2 ≥ 2 ⇒ OK", 3, 3, false},
		{"N=3 with 2 running, stopping one leaves 1 < 2 ⇒ break", 3, 2, true},
		{"N=5 with all 5 running, stopping one leaves 4 ≥ 3 ⇒ OK", 5, 5, false},
		{"N=5 with 3 running, stopping one leaves 2 < 3 ⇒ break", 5, 3, true},
		{"N=4 with all 4 running, stopping one leaves 3 ≥ 3 ⇒ OK", 4, 4, false},
		{"N=4 with 3 running, stopping one leaves 2 < 3 ⇒ break", 4, 3, true},
		{"N=6 with 5 running, stopping one leaves 4 ≥ 4 ⇒ OK", 6, 5, false},
		{"N=6 with 4 running, stopping one leaves 3 < 4 ⇒ break", 6, 4, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := rebalance.NewQuorum()
			q.AddSpec("dep", "svc", c.n)
			for i := 0; i < c.runningMembers; i++ {
				q.AddRunning(id(i), "dep", "svc")
			}
			got := q.WouldBreakQuorum(id(0), "node-a", "node-b")
			if got != c.wantBreaks {
				t.Errorf("WouldBreakQuorum() = %v, want %v (N=%d, running=%d, floor=%d)",
					got, c.wantBreaks, c.n, c.runningMembers, c.n/2+1)
			}
		})
	}
}

// TestQuorum_UnknownReplicaIsNotBlocked — a replica id the quorum
// view has never seen returns false. The rebalancer's hard filter
// catches unknown candidates separately; returning true here would
// mask that bug as "quorum-blocked".
func TestQuorum_UnknownReplicaIsNotBlocked(t *testing.T) {
	q := rebalance.NewQuorum()
	q.AddSpec("dep", "svc", 3)
	q.AddRunning("dep-svc-0", "dep", "svc")
	q.AddRunning("dep-svc-1", "dep", "svc")
	q.AddRunning("dep-svc-2", "dep", "svc")
	if got := q.WouldBreakQuorum("dep-other-0", "node-a", "node-b"); got {
		t.Errorf("WouldBreakQuorum(unknown id) = true, want false")
	}
}

// TestQuorum_PerServiceIndependence — replicas of service A do NOT
// count toward service B's quorum. Each service is its own group.
func TestQuorum_PerServiceIndependence(t *testing.T) {
	q := rebalance.NewQuorum()
	q.AddSpec("dep", "alpha", 3)
	q.AddSpec("dep", "beta", 3)
	// 3 RUNNING of alpha, only 2 RUNNING of beta.
	q.AddRunning("dep-alpha-0", "dep", "alpha")
	q.AddRunning("dep-alpha-1", "dep", "alpha")
	q.AddRunning("dep-alpha-2", "dep", "alpha")
	q.AddRunning("dep-beta-0", "dep", "beta")
	q.AddRunning("dep-beta-1", "dep", "beta")

	// Removing one alpha: 3→2 ≥ floor=2 ⇒ OK.
	if q.WouldBreakQuorum("dep-alpha-0", "node-a", "node-b") {
		t.Errorf("alpha removal should not break quorum (3 running, floor 2)")
	}
	// Removing one beta: 2→1 < floor=2 ⇒ break.
	if !q.WouldBreakQuorum("dep-beta-0", "node-a", "node-b") {
		t.Errorf("beta removal should break quorum (2 running, floor 2)")
	}
}

// TestQuorum_RebalancerCycleDoesNotBreakIt — drive a realistic
// cycle through the rig: 3-replica service spread across 3 nodes,
// hot node holds one replica. Quorum check passes (3→2 ≥ 2).
func TestQuorum_RebalancerCycleDoesNotBreakIt(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1 // skip hysteresis for this test
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	r.seedNode("node-c")
	// PACK placement so anti-affinity does not block a move onto a
	// node that already hosts a same-service replica — this test
	// targets the quorum check, not the SPREAD gate.
	r.seedDeploymentMode("dep", 3, pb.ServiceSpec_PLACEMENT_MODE_PACK)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedReplica("dep-web-1", "dep", "web", "node-b", 1)
	r.seedReplica("dep-web-2", "dep", "web", "node-c", 2)
	r.seedObserved("dep-web-0", "node-a")
	r.seedObserved("dep-web-1", "node-b")
	r.seedObserved("dep-web-2", "node-c")

	// Make node-a hot, others cool. Footprint relieves the dominant
	// CPU dimension by 0.3 — more than ReliefFloor 0.10.
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})
	r.source.setNode("node-c", rebalance.Snapshot{CPU: 0.3})
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})

	r.rebal.Cycle(context.TODO())
	// A move should have committed off node-a (3 RUNNING ≥ floor 2 → quorum OK).
	if got := r.replicaHost("dep-web-0"); got == "node-a" {
		t.Errorf("dep-web-0 was not moved off node-a; still on %q", got)
	}
}

// TestQuorum_BlocksMoveWhenWouldBreakFloor — 3-replica service with
// only 2 currently RUNNING. The hot replica is one of them; moving
// it would drop running members to 1 < floor 2. The cycle must
// emit a SKIPPED audit with reason=would_break_quorum and leave the
// replica in place.
func TestQuorum_BlocksMoveWhenWouldBreakFloor(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	// PACK placement so SPREAD's per-host anti-affinity isn't the
	// gate that fires first — this test targets the quorum check.
	r.seedDeploymentMode("dep", 3, pb.ServiceSpec_PLACEMENT_MODE_PACK)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedReplica("dep-web-1", "dep", "web", "node-b", 1)
	// Only 2 replicas observed RUNNING — quorum floor for N=3 is 2,
	// post-stop would be 1.
	r.seedObserved("dep-web-0", "node-a")
	r.seedObserved("dep-web-1", "node-b")

	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})

	r.rebal.Cycle(context.TODO())
	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("dep-web-0 should not have moved (quorum break); now on %q", got)
	}
	// Look for a SKIPPED audit with reason=would_break_quorum.
	found := false
	for _, ev := range r.rebalanceAuditsByType(pbAuditSkipped()) {
		if ev.GetPayload()["reason"] == "would_break_quorum" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected SKIPPED audit with reason=would_break_quorum")
	}
}

func id(i int) string {
	return "dep-svc-" + string(rune('0'+i))
}
