package rebalance_test

import (
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// hotRig builds a 2-node setup with one movable replica on node-a.
// Pressure on node-a is configurable; node-b sits at 0.2 (well
// below the trigger threshold). cfg overrides should be applied
// AFTER newRig sees DefaultConfig — call hotRig with the cfg you
// want.
func hotRig(t *testing.T, cfg rebalance.Config) *rig {
	t.Helper()
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	r.seedDeployment("dep", 1)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedObserved("dep-web-0", "node-a")
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})
	return r
}

// TestHysteresis_SingleSpikeDoesNotTrigger — one cycle at 0.95
// after a calm period is not enough. ConsecutiveCycles=2 must be
// satisfied, AND the EWMA must actually exceed the threshold —
// which a single 30s sample from 0.2 cannot achieve.
func TestHysteresis_SingleSpikeDoesNotTrigger(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.CycleInterval = 30 * time.Second
	cfg.ConsecutiveCycles = 2
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := hotRig(t, cfg)

	// Cycle 1: calm baseline so EWMAs seed at 0.2 for both.
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.2})
	r.rebal.Cycle(nil)
	if r.replicaHost("dep-web-0") != "node-a" {
		t.Fatalf("baseline cycle moved replica unexpectedly")
	}

	// Cycle 2: single huge spike — but only one cycle's worth of
	// elapsed time means alpha is small (~0.095) and the EWMA
	// barely budges from 0.2.
	r.advance(30 * time.Second)
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.rebal.Cycle(nil)
	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("single spike triggered a move; replica now on %q", got)
	}
}

// TestHysteresis_SustainedPressureEventuallyTriggers — feed 0.95
// to node-a for many cycles; once the EWMA crosses the threshold
// AND consecutive-over counter reaches ConsecutiveCycles, a move
// commits.
func TestHysteresis_SustainedPressureEventuallyTriggers(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.CycleInterval = 30 * time.Second
	cfg.ConsecutiveCycles = 2
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := hotRig(t, cfg)

	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	moved := false
	for i := 0; i < 60; i++ {
		r.rebal.Cycle(nil)
		if r.replicaHost("dep-web-0") != "node-a" {
			moved = true
			break
		}
		r.advance(30 * time.Second)
	}
	if !moved {
		t.Errorf("sustained pressure did not trigger any move in 60 cycles (30m simulated)")
	}
}

// TestHysteresis_DstCapBlocksMove — node-b is already at 0.6
// pressure; moving a 0.3 footprint there would push it to ~0.9 >
// DstCap 0.75. Cycle must skip with reason=dst_cap.
func TestHysteresis_DstCapBlocksMove(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1 // bypass hysteresis, isolate the dst-cap path
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := hotRig(t, cfg)
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.6})

	r.rebal.Cycle(nil)
	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("dst_cap should have blocked move; replica now on %q", got)
	}
	if !hasSkippedReason(r, "dst_cap") {
		t.Errorf("expected SKIPPED audit with reason=dst_cap")
	}
}

// TestHysteresis_ReliefFloorBlocksMove — replica footprint is tiny
// (0.05 CPU) and would deliver relief 0.05 < ReliefFloor 0.10.
// Cycle must skip with reason=relief_floor.
func TestHysteresis_ReliefFloorBlocksMove(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := hotRig(t, cfg)
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.05, Memory: 0.01})
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})

	r.rebal.Cycle(nil)
	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("relief_floor should have blocked move; replica now on %q", got)
	}
	if !hasSkippedReason(r, "relief_floor") {
		t.Errorf("expected SKIPPED audit with reason=relief_floor")
	}
}

// TestHysteresis_CooldownReplicaBlocksRepeatMove — first cycle
// commits a move; immediately rerunning hits the per-replica
// cooldown and emits a SKIPPED audit.
func TestHysteresis_CooldownReplicaBlocksRepeatMove(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 10 * time.Minute
	cfg.CooldownNode = 0
	r := hotRig(t, cfg)
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})

	// First cycle moves.
	r.rebal.Cycle(nil)
	if r.replicaHost("dep-web-0") == "node-a" {
		t.Fatalf("first cycle did not commit a move")
	}
	// Re-hot node-a (the replica is gone, but we're simulating a
	// second replica's worth of pressure landing back). Add a new
	// replica on node-a to give the cycle a candidate to consider.
	r.seedReplica("dep-web-1", "dep", "web", "node-a", 1)
	r.seedObserved("dep-web-1", "node-a")
	r.source.setReplica("dep-web-1", rebalance.Footprint{CPU: 0.3, Memory: 0.1})

	// Stamp the previously-moved replica back onto node-a too, so
	// the cooldown check applies — but the new replica should still
	// be eligible. (We only assert the moved one stays put.)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})

	r.advance(1 * time.Minute) // well inside the 10m cooldown
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.rebal.Cycle(nil)
	if r.replicaHost("dep-web-0") != "node-a" {
		t.Errorf("dep-web-0 was moved despite cooldown_replica")
	}
	if !hasSkippedReason(r, "cooldown_replica") {
		t.Errorf("expected SKIPPED audit with reason=cooldown_replica")
	}
}

// TestHysteresis_CooldownNodeBlocksSecondMoveOntoDst — first cycle
// moves replica A onto node-b. Second cycle has another hot replica
// on node-a, but node-b is in its cooldown window so the move is
// skipped with reason=cooldown_node.
func TestHysteresis_CooldownNodeBlocksSecondMoveOntoDst(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 2 * time.Minute
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	r.seedNode("node-c")
	// PACK so anti-affinity does not block the second move onto
	// node-b on its own — we want cooldown_node to be the operative
	// gate. Use a 3-replica service (floor=2, members=3 → post=2 ≥
	// floor) so the quorum gate doesn't fire first; with 2 replicas
	// the v0 "all-replicas-of-one-service" quorum model would
	// conservatively block any move.
	r.seedDeploymentMode("dep", 3, pb.ServiceSpec_PLACEMENT_MODE_PACK)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedReplica("dep-web-1", "dep", "web", "node-a", 1)
	r.seedReplica("dep-web-2", "dep", "web", "node-c", 2)
	r.seedObserved("dep-web-0", "node-a")
	r.seedObserved("dep-web-1", "node-a")
	r.seedObserved("dep-web-2", "node-c")
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})
	r.source.setReplica("dep-web-1", rebalance.Footprint{CPU: 0.3, Memory: 0.1})
	r.source.setReplica("dep-web-2", rebalance.Footprint{CPU: 0.1, Memory: 0.1})
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})
	// node-c is also a viable destination — first cycle will pick
	// one based on score+tiebreak; second cycle exercises the
	// cooldown_node skip on whichever node-b or node-c was chosen.
	r.source.setNode("node-c", rebalance.Snapshot{CPU: 0.2})

	// Cycle 1: moves one of the node-a replicas to node-b or node-c.
	r.rebal.Cycle(nil)
	moved := r.replicaHost("dep-web-0") != "node-a" || r.replicaHost("dep-web-1") != "node-a"
	if !moved {
		t.Fatalf("first cycle did not commit a move; hosts: dep-web-0=%q, dep-web-1=%q",
			r.replicaHost("dep-web-0"), r.replicaHost("dep-web-1"))
	}

	// Advance 30s — well inside the 2m node cooldown.
	r.advance(30 * time.Second)
	// Sustain pressure on node-a (still has one replica left).
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})

	// Cycle 2: should refuse a second move onto whichever node was
	// the dst in cycle 1 (cooldown_node fires).
	r.rebal.Cycle(nil)
	if !hasSkippedReason(r, "cooldown_node") {
		t.Errorf("expected SKIPPED audit with reason=cooldown_node in second cycle")
	}
}

// TestHysteresis_ImbalanceGapBlocksMove — both nodes pressured
// roughly equally; max - min < ImbalanceGap 0.25, so no move
// commits even though both EWMAs exceed the trigger threshold.
func TestHysteresis_ImbalanceGapBlocksMove(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := hotRig(t, cfg)
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.9})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.85})

	r.rebal.Cycle(nil)
	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("imbalance gap should have blocked move; replica now on %q", got)
	}
}

// hasSkippedReason scans every REBALANCE_SKIPPED audit for one
// whose payload reason matches.
func hasSkippedReason(r *rig, want string) bool {
	for _, ev := range r.rebalanceAuditsByType(pbAuditSkipped()) {
		if ev.GetPayload()["reason"] == want {
			return true
		}
	}
	return false
}
