// Package rebalance is the leader-only pressure-based rebalancer
// (issue #92, ADR 0002). It observes per-node CPU + memory pressure
// supplied by a pluggable PressureSource, picks the cheapest stateless
// replica to move off the hottest node when the cluster is meaningfully
// imbalanced, and emits a single move per cycle through the existing
// scheduler raft-Apply path (ReplicaDesiredUpsert with a new Host).
//
// The rebalancer is always-on — it spawns on every daemon, self-gates
// on raft leader status, and runs with built-in defaults. There is no
// operator opt-in / opt-out and no dry-run mode: the defaults bias so
// strongly toward inaction (sustained-pressure threshold AND a cross-
// node imbalance gap must both hold) that an idle cluster never moves
// anything, and when moves do happen they are conservative single
// stateless restarts that the existing scheduler reconcile loop knows
// how to execute.
//
// This package contains no real signal collector. PressureSource is a
// pure interface; tests inject a fake, and the daemon currently passes
// a NoopSource that returns "no pressure data" for every node — wiring
// real cgroup v2 collection is a follow-up. Until that lands the
// rebalancer is effectively dormant: gates short-circuit on absent
// data before any move is considered.
package rebalance

import "time"

// Config holds the rebalancer's hysteresis knobs. Every field has a
// documented default in DefaultConfig. There is no operator-facing
// config block for this — the daemon constructs Config from
// DefaultConfig() at boot. Knobs exist so tests can drive the loop on
// non-default cadences; bake-in values are the supported production
// posture.
type Config struct {
	// TriggerThreshold is the composite-pressure value a node must
	// exceed for ConsecutiveCycles in a row before the rebalancer
	// considers moves. Default: 0.85.
	TriggerThreshold float64

	// ImbalanceGap is the minimum max-pressure − min-pressure
	// across nodes required to trigger a move. Stops the rebalancer
	// from churning when the whole cluster is uniformly busy.
	// Default: 0.25.
	ImbalanceGap float64

	// ReliefFloor is the minimum reduction in src pressure a
	// candidate move must deliver, computed as
	// `pre_pressure(src) − post_pressure(src) ≥ ReliefFloor`.
	// Default: 0.10.
	ReliefFloor float64

	// DstCap is the maximum post-move pressure permitted on the
	// destination node. A candidate that would push dst past this is
	// filtered. Default: 0.75.
	DstCap float64

	// CooldownReplica is the minimum age a replica must have on its
	// current host before it becomes movable again. Prevents the
	// rebalancer from re-moving something it just moved. Default:
	// 10m.
	CooldownReplica time.Duration

	// CooldownNode is the minimum time a destination node must wait
	// after receiving a migration before it accepts another.
	// Default: 2m.
	CooldownNode time.Duration

	// CycleInterval is the rebalancer's tick. The composite pressure
	// is sampled once per tick and fed into the per-node EWMA.
	// Default: 30s.
	CycleInterval time.Duration

	// ConsecutiveCycles is how many cycles in a row the hot node's
	// composite must stay above TriggerThreshold before a move can
	// commit. Default: 2 (≈1 minute at the default 30s tick).
	ConsecutiveCycles int
}

// DefaultConfig returns the documented defaults: 0.85 trigger / 0.25
// gap / 0.10 relief floor / 0.75 dst cap, 10m replica cooldown / 2m
// node cooldown, 30s cycle, 2 consecutive cycles.
func DefaultConfig() Config {
	return Config{
		TriggerThreshold:  0.85,
		ImbalanceGap:      0.25,
		ReliefFloor:       0.10,
		DstCap:            0.75,
		CooldownReplica:   10 * time.Minute,
		CooldownNode:      2 * time.Minute,
		CycleInterval:     30 * time.Second,
		ConsecutiveCycles: 2,
	}
}
