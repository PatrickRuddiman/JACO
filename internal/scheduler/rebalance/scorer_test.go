package rebalance_test

import (
	"math"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// candidate is a minimal builder for MoveCandidate values used in the
// scorer/filter table tests below. Defaults are "stateless replica,
// CPU-dominant hotspot at src, plenty of room at dst, anti-affinity
// OK". Tests override only the fields they care about — keeps the
// rows readable.
func candidate(mut ...func(c *rebalance.MoveCandidate)) *rebalance.MoveCandidate {
	c := &rebalance.MoveCandidate{
		Replica: &pb.ReplicaDesired{
			Id: "dep-web-0", Deployment: "dep", Service: "web", Host: "node-a", Index: 0,
		},
		Src: "node-a",
		Dst: "node-b",
		Spec: &pb.ServiceSpec{
			Name: "web", Replicas: 3,
			Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
		},
		Footprint:       rebalance.Footprint{CPU: 0.2, Memory: 0.1},
		SrcPressure:     rebalance.Snapshot{CPU: 0.9, Memory: 0.3},
		DstPressure:     rebalance.Snapshot{CPU: 0.2, Memory: 0.2},
		Priority:        1,
		PerHostCount:    0,
		DstResourceFits: true,
	}
	for _, m := range mut {
		m(c)
	}
	return c
}

// TestScore_ReliefEstimate_PicksDominantDimension — under a CPU
// hotspot, the relief estimate is the replica's CPU footprint; under
// memory, the memory footprint.
func TestScore_ReliefEstimate_PicksDominantDimension(t *testing.T) {
	cpuHot := candidate(func(c *rebalance.MoveCandidate) {
		c.SrcPressure = rebalance.Snapshot{CPU: 0.9, Memory: 0.2}
		c.Footprint = rebalance.Footprint{CPU: 0.4, Memory: 0.05}
	})
	memHot := candidate(func(c *rebalance.MoveCandidate) {
		c.SrcPressure = rebalance.Snapshot{CPU: 0.2, Memory: 0.9}
		c.Footprint = rebalance.Footprint{CPU: 0.05, Memory: 0.4}
	})
	scpu := rebalance.Score(cpuHot, rebalance.DefaultConfig())
	smem := rebalance.Score(memHot, rebalance.DefaultConfig())
	// Both should score nearly identically (relief 0.4, stateless 2.0,
	// prio 1, cost ~0.01): ≈ 0.79.
	if math.Abs(scpu-smem) > 1e-6 {
		t.Errorf("CPU-hot score (%v) and Memory-hot score (%v) should match symmetrically", scpu, smem)
	}
	if scpu < 0.7 {
		t.Errorf("Score = %v, want roughly 0.79", scpu)
	}
}

// TestScore_StatelessRanksAboveStateful — same relief / priority /
// move-cost; stateless bonus 2.0 vs stateful 1.0 doubles the
// numerator. Verifies the bias ADR §"Selection policy" requires.
// (We construct the stateful candidate by bypassing HardFilter —
// Score itself isn't supposed to reject stateful, only HardFilter
// does.)
func TestScore_StatelessRanksAboveStateful(t *testing.T) {
	stateless := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint = rebalance.Footprint{CPU: 0.3, Memory: 0.1, Stateful: false}
	})
	stateful := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint = rebalance.Footprint{CPU: 0.3, Memory: 0.1, Bytes: 1024 * 1024, Stateful: true}
	})
	sl := rebalance.Score(stateless, rebalance.DefaultConfig())
	st := rebalance.Score(stateful, rebalance.DefaultConfig())
	if sl <= st {
		t.Errorf("stateless score (%v) should be > stateful score (%v)", sl, st)
	}
}

// TestScore_PriorityInverse_LowerPriorityRanksHigher — between two
// otherwise identical candidates, the higher-priority replica
// (larger priority number) scores LESS, so the rebalancer prefers
// moving the lower-priority one.
func TestScore_PriorityInverse_LowerPriorityRanksHigher(t *testing.T) {
	low := candidate(func(c *rebalance.MoveCandidate) { c.Priority = 1 })
	high := candidate(func(c *rebalance.MoveCandidate) { c.Priority = 10 })
	sLow := rebalance.Score(low, rebalance.DefaultConfig())
	sHigh := rebalance.Score(high, rebalance.DefaultConfig())
	if sLow <= sHigh {
		t.Errorf("priority=1 score (%v) should be > priority=10 score (%v)", sLow, sHigh)
	}
}

// TestScore_MoveCost_StatefulBytesPenalty — between two stateful
// candidates differing only in volume size, the larger volume scores
// strictly lower (its move-cost is bytes / 50 MB/s). The check is
// directional, not exact, so an operator tweaking the rate constant
// in a follow-up doesn't have to update this test.
func TestScore_MoveCost_StatefulBytesPenalty(t *testing.T) {
	small := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint = rebalance.Footprint{CPU: 0.1, Memory: 0.1, Bytes: 1 * 1024 * 1024, Stateful: true}
	})
	large := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint = rebalance.Footprint{CPU: 0.1, Memory: 0.1, Bytes: 20 * 1024 * 1024 * 1024, Stateful: true}
	})
	sSmall := rebalance.Score(small, rebalance.DefaultConfig())
	sLarge := rebalance.Score(large, rebalance.DefaultConfig())
	if sSmall <= sLarge {
		t.Errorf("small-volume score (%v) should be > large-volume score (%v)", sSmall, sLarge)
	}
}

// TestPostMovePressure_SubtractsFromSrcAddsToDst — sanity that the
// arithmetic the cycle uses for dst_cap / relief_floor gates matches
// the footprint.
func TestPostMovePressure_SubtractsFromSrcAddsToDst(t *testing.T) {
	c := candidate(func(c *rebalance.MoveCandidate) {
		c.SrcPressure = rebalance.Snapshot{CPU: 0.9, Memory: 0.5, ReplicaCount: 4, ReplicaSoftCap: 50}
		c.DstPressure = rebalance.Snapshot{CPU: 0.2, Memory: 0.1, ReplicaCount: 1, ReplicaSoftCap: 50}
		c.Footprint = rebalance.Footprint{CPU: 0.3, Memory: 0.2}
	})
	postSrc, postDst := rebalance.PostMovePressure(c)
	if math.Abs(postSrc.CPU-0.6) > 1e-9 {
		t.Errorf("postSrc.CPU = %v, want 0.6", postSrc.CPU)
	}
	if math.Abs(postSrc.Memory-0.3) > 1e-9 {
		t.Errorf("postSrc.Memory = %v, want 0.3", postSrc.Memory)
	}
	if postSrc.ReplicaCount != 3 {
		t.Errorf("postSrc.ReplicaCount = %d, want 3", postSrc.ReplicaCount)
	}
	if math.Abs(postDst.CPU-0.5) > 1e-9 {
		t.Errorf("postDst.CPU = %v, want 0.5", postDst.CPU)
	}
	if math.Abs(postDst.Memory-0.3) > 1e-9 {
		t.Errorf("postDst.Memory = %v, want 0.3", postDst.Memory)
	}
	if postDst.ReplicaCount != 2 {
		t.Errorf("postDst.ReplicaCount = %d, want 2", postDst.ReplicaCount)
	}
}

// TestHardFilter_OrderingAndReasons — explicit table over each gate.
// Order matters: stateful_filtered must come before resource_limits
// must come before anti_affinity must come before would_break_quorum,
// per ADR §"Selection policy" ordering. (The order is observable
// through the reason string the cycle records in the audit log.)
func TestHardFilter_OrderingAndReasons(t *testing.T) {
	cases := []struct {
		name string
		mut  func(c *rebalance.MoveCandidate)
		want rebalance.SkipReason
	}{
		{
			name: "happy path passes",
			mut:  func(c *rebalance.MoveCandidate) {},
			want: rebalance.SkipNone,
		},
		{
			name: "stateful rejected first",
			mut: func(c *rebalance.MoveCandidate) {
				c.Footprint.Stateful = true
				c.DstResourceFits = false // would also fail, stateful comes first
			},
			want: rebalance.SkipStatefulFiltered,
		},
		{
			name: "resource limits rejected before anti-affinity",
			mut: func(c *rebalance.MoveCandidate) {
				c.DstResourceFits = false
				c.PerHostCount = 1 // would also trip spread, resource comes first
			},
			want: rebalance.SkipResourceLimits,
		},
		{
			name: "anti-affinity SPREAD rejects co-location",
			mut: func(c *rebalance.MoveCandidate) {
				c.PerHostCount = 1
			},
			want: rebalance.SkipAntiAffinity,
		},
		{
			name: "anti-affinity HOSTS rejects non-listed dst",
			mut: func(c *rebalance.MoveCandidate) {
				c.Spec.Placement = pb.ServiceSpec_PLACEMENT_MODE_HOSTS
				c.Spec.Hosts = []string{"node-c", "node-d"}
			},
			want: rebalance.SkipAntiAffinity,
		},
		{
			name: "anti-affinity HOSTS accepts listed dst",
			mut: func(c *rebalance.MoveCandidate) {
				c.Spec.Placement = pb.ServiceSpec_PLACEMENT_MODE_HOSTS
				c.Spec.Hosts = []string{"node-b"}
			},
			want: rebalance.SkipNone,
		},
		{
			name: "GLOBAL never moves",
			mut: func(c *rebalance.MoveCandidate) {
				c.Spec.Placement = pb.ServiceSpec_PLACEMENT_MODE_GLOBAL
			},
			want: rebalance.SkipAntiAffinity,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate(tc.mut)
			if got := rebalance.HardFilter(c, nil); got != tc.want {
				t.Errorf("HardFilter = %q, want %q", got, tc.want)
			}
		})
	}
}
