package rebalance

import (
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// MoveCandidate is one (replica, src, dst) the scorer evaluates. The
// rebalancer enumerates candidates per-cycle from the most-pressured
// node; the scorer hard-filters then ranks. Replica is read-only; the
// rebalancer keeps it pointing at the live state.ReplicaDesired entry.
type MoveCandidate struct {
	Replica *pb.ReplicaDesired
	Src     string
	Dst     string

	// Spec is the service spec for Replica; the scorer reads
	// Replicas for the priority-inverse weight and Placement for
	// anti-affinity checks. Anti-affinity v0 enforces only
	// PLACEMENT_MODE_HOSTS (dst must be in Spec.Hosts when set);
	// PLACEMENT_MODE_SPREAD is informational (the scheduler already
	// spreads on initial placement, and a rebalance move that keeps
	// at most one replica per dst preserves that — see PerHostCount
	// below).
	Spec *pb.ServiceSpec

	// Footprint is the replica's resource cost on Src, used both
	// for the relief estimate and as the post-move offset applied
	// to dst's pressure.
	Footprint Footprint

	// SrcPressure / DstPressure are the EWMA-smoothed composite
	// scores for Src / Dst going into the cycle.
	SrcPressure Snapshot
	DstPressure Snapshot

	// Priority is the service-declared priority used for the
	// priority-inverse weight. 0 (UNSPECIFIED) is normalised to 1 so
	// `1 / priority` is well-defined for the default case.
	Priority int

	// PerHostCount is the current count of Spec's replicas already
	// pinned to Dst. Used for the anti-affinity SPREAD check (won't
	// place a second replica of the same service on dst).
	PerHostCount int

	// DstResourceFits reports whether Dst can host the replica
	// without violating Dst-side resource limits. The rebalancer
	// fills this from a simple "Dst's post-move CPU/Memory utilisation
	// would stay ≤ 1.0" check; richer per-replica limit modelling
	// (memory_limit, cpus) lives in the runtime, not here.
	DstResourceFits bool
}

// SkipReason is the audit reason string when a candidate is dropped or
// the cycle commits no move. Stable wire strings — operators grep
// these in the audit log.
type SkipReason string

const (
	SkipNone              SkipReason = ""
	SkipStatefulFiltered  SkipReason = "stateful_filtered"
	SkipResourceLimits    SkipReason = "resource_limits"
	SkipAntiAffinity      SkipReason = "anti_affinity"
	SkipWouldBreakQuorum  SkipReason = "would_break_quorum"
	SkipCooldownReplica   SkipReason = "cooldown_replica"
	SkipCooldownNode      SkipReason = "cooldown_node"
	SkipDstCap            SkipReason = "dst_cap"
	SkipReliefFloor       SkipReason = "relief_floor"
	SkipNoEligibleDst     SkipReason = "no_eligible_dst"
	SkipNoCandidate       SkipReason = "no_candidate"
)

// statelessBonus and statefulBonus implement ADR §"Selection policy":
//
//	stateless_bonus = 2.0  (prefer cheap moves)
//	stateful_bonus  = 1.0
//
// Kept as named constants rather than a struct field because v0 has no
// operator knob for them — the ratio is a design decision baked into
// the rebalancer's bias toward stateless moves, not a tunable.
const (
	statelessBonus = 2.0
	statefulBonus  = 1.0
)

// statelessMoveCost is the small fixed restart-cost penalty applied to
// every stateless move. v0 uses an arbitrary small constant; the only
// invariant that matters is "stateless cost < stateful cost" so the
// scorer prefers stateless under equivalent relief.
const statelessMoveCost = 0.01

// statefulBytesPerSecond is the move-rate assumption used to convert
// Footprint.Bytes to a time-shaped cost. Used only when Stateful=true
// (in which case the stateful filter also rejects today, so this is
// just the formula that will start applying when a remote-volume
// backend lands and the filter flips off; see #135).
const statefulBytesPerSecond = 50 * 1024 * 1024

// HardFilter returns the first reason the candidate is unmovable, or
// SkipNone when it survives every gate. Ordering matches the ADR's
// "before scoring" list. This is called BEFORE Score.
func HardFilter(c *MoveCandidate, q *Quorum) SkipReason {
	if c.Footprint.Stateful {
		// Stateful is filtered today because JACO has no way to
		// re-attach a replica's data on a different node. A remote-
		// mounted volume backend (#135) would unblock this.
		return SkipStatefulFiltered
	}
	if !c.DstResourceFits {
		return SkipResourceLimits
	}
	if !antiAffinityOK(c) {
		return SkipAntiAffinity
	}
	if q != nil && q.WouldBreakQuorum(c.Replica.GetId(), c.Src, c.Dst) {
		return SkipWouldBreakQuorum
	}
	return SkipNone
}

// antiAffinityOK returns false when placing this candidate on Dst
// would violate the service's PlacementMode. v0 checks:
//
//   - PLACEMENT_MODE_HOSTS: Dst must be in Spec.Hosts.
//   - PLACEMENT_MODE_SPREAD: Dst must NOT already host another
//     replica of the same service. PerHostCount is the count of
//     same-service replicas already on Dst from state.ReplicasDesired
//     EXCLUDING the candidate; if it's > 0, the move would co-locate.
//   - PLACEMENT_MODE_PACK: no anti-affinity constraint by definition
//     (pack happily stacks).
//   - PLACEMENT_MODE_GLOBAL: never moved by the rebalancer
//     (daemonsets are one-per-host by definition); a global service
//     that somehow reaches the scorer is filtered here.
func antiAffinityOK(c *MoveCandidate) bool {
	if c.Spec == nil {
		return true
	}
	switch c.Spec.GetPlacement() {
	case pb.ServiceSpec_PLACEMENT_MODE_HOSTS:
		for _, h := range c.Spec.GetHosts() {
			if h == c.Dst {
				return true
			}
		}
		return false
	case pb.ServiceSpec_PLACEMENT_MODE_SPREAD:
		return c.PerHostCount == 0
	case pb.ServiceSpec_PLACEMENT_MODE_GLOBAL:
		return false
	default: // PACK, UNSPECIFIED
		return true
	}
}

// Score implements the ADR §"Selection policy" formula:
//
//	score = relief_estimate * stateless_bonus * priority_inverse - move_cost
//
// where relief_estimate is the candidate's contribution to Src's
// dominant pressure dimension and move_cost is restart-cost (stateless)
// or bytes-to-ship-at-50MB/s (stateful, currently unreachable post
// HardFilter).
//
// Score is ONLY called on candidates that survived HardFilter; the
// scorer assumes Replica + Footprint are populated and Stateful=false.
func Score(c *MoveCandidate, _ Config) float64 {
	relief := reliefEstimate(c)
	bonus := statelessBonus
	if c.Footprint.Stateful {
		bonus = statefulBonus
	}
	prio := c.Priority
	if prio <= 0 {
		prio = 1
	}
	priorityInverse := 1.0 / float64(prio)
	return relief*bonus*priorityInverse - moveCost(c)
}

// reliefEstimate is the replica's contribution to Src's dominant
// pressure dimension. CPU-dominant hotspot → replica CPU footprint.
// Memory-dominant → memory footprint. DiskIO- or Count-dominant
// hotspots fall back to whichever of CPU/Memory the footprint reports
// largest (a count-driven node is relieved by ANY move of any size).
func reliefEstimate(c *MoveCandidate) float64 {
	switch Dominant(c.SrcPressure) {
	case DimMemory:
		return c.Footprint.Memory
	case DimCPU:
		return c.Footprint.CPU
	default: // DimDiskIO, DimCount
		if c.Footprint.CPU > c.Footprint.Memory {
			return c.Footprint.CPU
		}
		return c.Footprint.Memory
	}
}

// moveCost returns the scorer's estimate of how expensive the move
// is. Stateless: fixed small restart-cost. Stateful: bytes / 50MB/s
// in seconds (so a 1 GB volume scores roughly 20 vs 0.01 for a
// stateless move — the scorer prefers the redis cache by ~3
// orders of magnitude).
func moveCost(c *MoveCandidate) float64 {
	if !c.Footprint.Stateful {
		return statelessMoveCost
	}
	if c.Footprint.Bytes <= 0 {
		return statelessMoveCost
	}
	return float64(c.Footprint.Bytes) / float64(statefulBytesPerSecond)
}

// PostMovePressure returns (post_src, post_dst) composite pressure
// after the move lands. Computed by subtracting the replica's CPU /
// Memory footprint from Src's pre-move dimensions and adding to Dst's.
// Replica count moves by 1 in each direction. DiskIO doesn't move
// (the rebalancer has no per-replica disk footprint in v0 — the
// PressureSource follow-up adds it).
//
// Exposed for unit tests that drive the relief/dst-cap gates without
// going through Cycle.
func PostMovePressure(c *MoveCandidate) (post Snapshot, postDst Snapshot) {
	post = c.SrcPressure
	postDst = c.DstPressure

	post.CPU -= c.Footprint.CPU
	if post.CPU < 0 {
		post.CPU = 0
	}
	post.Memory -= c.Footprint.Memory
	if post.Memory < 0 {
		post.Memory = 0
	}
	if post.ReplicaCount > 0 {
		post.ReplicaCount--
	}

	postDst.CPU += c.Footprint.CPU
	postDst.Memory += c.Footprint.Memory
	postDst.ReplicaCount++
	return post, postDst
}
