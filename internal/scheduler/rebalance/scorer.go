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
	// Placement for anti-affinity checks.
	Spec *pb.ServiceSpec

	// Footprint is the replica's resource cost on Src, used both
	// for the relief estimate and as the post-move offset applied
	// to dst's pressure.
	Footprint Footprint

	// SrcPressure / DstPressure are the EWMA-smoothed snapshots for
	// Src / Dst going into the cycle.
	SrcPressure Snapshot
	DstPressure Snapshot

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
	SkipNone            SkipReason = ""
	SkipResourceLimits  SkipReason = "resource_limits"
	SkipAntiAffinity    SkipReason = "anti_affinity"
	SkipCooldownReplica SkipReason = "cooldown_replica"
	SkipCooldownNode    SkipReason = "cooldown_node"
	SkipDstCap          SkipReason = "dst_cap"
	SkipReliefFloor     SkipReason = "relief_floor"
	SkipNoEligibleDst   SkipReason = "no_eligible_dst"
	SkipNoCandidate     SkipReason = "no_candidate"
)

// moveCost is the small fixed restart-cost penalty applied to every
// move. The only invariant that matters is that two candidates with
// identical relief still pick the same one deterministically (broken
// by replica id in the caller). A constant suffices.
const moveCost = 0.01

// HardFilter returns the first reason the candidate is unmovable, or
// SkipNone when it survives every gate. Ordering matches the ADR's
// "before scoring" list. This is called BEFORE Score.
func HardFilter(c *MoveCandidate) SkipReason {
	if !c.DstResourceFits {
		return SkipResourceLimits
	}
	if !antiAffinityOK(c) {
		return SkipAntiAffinity
	}
	return SkipNone
}

// antiAffinityOK returns false when placing this candidate on Dst
// would violate the service's PlacementMode:
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

// Score ranks survivors of HardFilter:
//
//	score = relief_estimate - move_cost
//
// where relief_estimate is the candidate's contribution to Src's
// dominant pressure dimension.
func Score(c *MoveCandidate) float64 {
	return reliefEstimate(c) - moveCost
}

// reliefEstimate is the replica's contribution to Src's dominant
// pressure dimension. CPU-dominant hotspot → replica CPU footprint.
// Memory-dominant → memory footprint.
func reliefEstimate(c *MoveCandidate) float64 {
	if Dominant(c.SrcPressure) == DimMemory {
		return c.Footprint.Memory
	}
	return c.Footprint.CPU
}

// PostMovePressure returns (post_src, post_dst) snapshots after the
// move lands. Computed by subtracting the replica's CPU / Memory
// footprint from Src's pre-move dimensions and adding to Dst's.
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

	postDst.CPU += c.Footprint.CPU
	postDst.Memory += c.Footprint.Memory
	return post, postDst
}
