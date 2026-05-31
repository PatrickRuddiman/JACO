// Package rebalance is the leader-only pressure-based rebalancer (issue
// #92, ADR 0002). It observes per-node pressure signals supplied by a
// pluggable PressureSource, picks the cheapest replica to move off the
// hottest node when the cluster is meaningfully imbalanced, and emits a
// single move per cycle through the existing scheduler raft-Apply path
// (ReplicaDesiredUpsert with a new Host).
//
// Defaults bias toward inaction: the rebalancer is disabled out of the
// box, and even when enabled both a sustained-pressure threshold AND a
// cross-node imbalance gap must hold before any move is considered. When
// the loop is configured but disabled it runs in DRY-RUN mode — every
// would-have-moved decision lands in the audit log
// (AUDIT_EVENT_TYPE_REBALANCE_DRY_RUN) so operators can evaluate the
// policy before flipping it on, and no commands hit the Applier.
//
// This package contains no real signal collector. PressureSource is a
// pure interface; tests inject a fake, and the daemon currently passes a
// stub that returns "no pressure data" for every node — wiring real
// cgroup/dockerx collection is a follow-up (NOT in this PR).
//
// Out of scope here:
//   - Stateful candidates: the hard filter rejects every replica whose
//     PressureSource.ReplicaFootprint reports Stateful=true. The
//     stateless_bonus knob in scorer.go stays in place so flipping the
//     filter off (once #91 lands) starts considering them without
//     further changes.
//   - Volume migration itself (#91).
package rebalance

import "time"

// Config holds the operator-tunable knobs from ADR 0002 §"Thresholds and
// hysteresis". Every field has a documented default in DefaultConfig;
// loading from jacod.yaml fills missing keys from there.
type Config struct {
	// Enabled flips the rebalancer between live (true) and dry-run
	// (false). The cycle loop runs in BOTH modes — dry-run still computes
	// candidates and emits audit events, it just never commits a move
	// command through Applier.
	Enabled bool

	// TriggerThreshold is the composite-pressure value a node must
	// exceed for ConsecutiveCycles in a row before the rebalancer
	// considers moves. ADR default: 0.85.
	TriggerThreshold float64

	// ImbalanceGap is the minimum max-pressure − min-pressure
	// across nodes required to trigger a move. Stops the rebalancer
	// from churning when the whole cluster is uniformly busy. ADR
	// default: 0.25.
	ImbalanceGap float64

	// ReliefFloor is the minimum reduction in src pressure a
	// candidate move must deliver. Computed as
	// `pre_pressure(src) − post_pressure(src) ≥ ReliefFloor`. ADR
	// default: 0.10.
	ReliefFloor float64

	// DstCap is the maximum post-move pressure permitted on the
	// destination node. A candidate that would push dst past this is
	// filtered. ADR default: 0.75.
	DstCap float64

	// CooldownReplica is the minimum age a replica must have on its
	// current host before it becomes movable again. Prevents the
	// rebalancer from re-moving something it just moved. ADR default:
	// 10m.
	CooldownReplica time.Duration

	// CooldownNode is the minimum time a destination node must wait
	// after receiving a migration before it accepts another. ADR
	// default: 2m.
	CooldownNode time.Duration

	// CycleInterval is the rebalancer's tick. The composite pressure
	// is sampled once per tick and fed into the per-node EWMA. ADR
	// default: 30s.
	CycleInterval time.Duration

	// ConsecutiveCycles is how many cycles in a row the hot node's
	// composite must stay above TriggerThreshold before a move can
	// commit. ADR §"Thresholds and hysteresis" specifies 2 cycles
	// (≈1 minute at the default 30s tick).
	ConsecutiveCycles int

	// ReplicaSoftCap is the per-node replica-count denominator used
	// when the count dimension dominates the pressure score
	// (replica_count / soft_cap). ADR §"Signals" calls this a
	// node-local config; v0 surfaces it as a single cluster-wide
	// knob with default 50.
	ReplicaSoftCap int
}

// DefaultConfig returns the ADR defaults: disabled (dry-run), 0.85
// trigger / 0.25 gap / 0.10 relief floor / 0.75 dst cap, 10m replica
// cooldown / 2m node cooldown, 30s cycle, 2 consecutive cycles, soft
// cap 50.
func DefaultConfig() Config {
	return Config{
		Enabled:           false,
		TriggerThreshold:  0.85,
		ImbalanceGap:      0.25,
		ReliefFloor:       0.10,
		DstCap:            0.75,
		CooldownReplica:   10 * time.Minute,
		CooldownNode:      2 * time.Minute,
		CycleInterval:     30 * time.Second,
		ConsecutiveCycles: 2,
		ReplicaSoftCap:    50,
	}
}
