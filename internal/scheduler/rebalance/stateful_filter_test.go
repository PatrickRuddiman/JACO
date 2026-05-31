package rebalance_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
)

// TestStatefulFilter_HardRejectsAtFootprint — the rebalancer must
// hard-filter every candidate whose Footprint.Stateful == true,
// regardless of how attractive the score would otherwise be. ADR
// 0002 §"Move execution" gates the stateful path on #91; flipping
// the filter off is a separate follow-up PR.
//
// The test sets up a 2-node hot/cool scenario, marks the replica
// stateful, and asserts:
//   1. The replica is NOT moved.
//   2. A SKIPPED audit lands with reason=stateful_filtered.
//   3. No ReplicaDesiredUpsert is committed via Applier.
func TestStatefulFilter_HardRejectsAtFootprint(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = true
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	r.seedDeployment("dep", 1)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedObserved("dep-web-0", "node-a")

	// Stateful footprint — should be hard-filtered even though the
	// scoring numbers would otherwise commit a move.
	r.source.setReplica("dep-web-0", rebalance.Footprint{
		CPU: 0.3, Memory: 0.1, Bytes: 1024 * 1024 * 1024, Stateful: true,
	})
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})

	r.rebal.Cycle(nil)

	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("stateful replica was moved off node-a; now on %q", got)
	}
	if !hasSkippedReason(r, "stateful_filtered") {
		t.Errorf("expected SKIPPED audit with reason=stateful_filtered")
	}
	// No MOVED audit and no replica upsert applies.
	if got := len(r.rebalanceAuditsByType(pbAuditMoved())); got != 0 {
		t.Errorf("MOVED audit count = %d, want 0", got)
	}
	if got := countReplicaUpserts(r, "dep-web-0"); got != 0 {
		t.Errorf("ReplicaDesiredUpsert applies = %d, want 0", got)
	}
}

// TestStatefulFilter_HardFilterTablePinsFilterOrder — the unit-level
// HardFilter check (no rig) confirms Stateful is the FIRST reason
// returned even when other gates would also reject. Important
// because the audit log carries the reason, and operators reading
// "stateful_filtered" know they hit the #91-gated branch — not a
// resource-limits / anti-affinity false positive.
func TestStatefulFilter_HardFilterTablePinsFilterOrder(t *testing.T) {
	c := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint.Stateful = true
		c.DstResourceFits = false // would also reject
		c.PerHostCount = 1        // would also reject SPREAD anti-affinity
	})
	if got := rebalance.HardFilter(c, nil); got != rebalance.SkipStatefulFiltered {
		t.Errorf("HardFilter on stateful+overdrawn+colocated = %q, want %q",
			got, rebalance.SkipStatefulFiltered)
	}
}
