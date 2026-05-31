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
	scpu := rebalance.Score(cpuHot)
	smem := rebalance.Score(memHot)
	// Both should score nearly identically (relief 0.4 - 0.01 = 0.39).
	if math.Abs(scpu-smem) > 1e-9 {
		t.Errorf("CPU-hot score (%v) and Memory-hot score (%v) should match symmetrically", scpu, smem)
	}
	want := 0.4 - 0.01
	if math.Abs(scpu-want) > 1e-9 {
		t.Errorf("Score = %v, want %v", scpu, want)
	}
}

// TestScore_BiggerFootprintScoresHigher — between two stateless
// candidates differing only in CPU footprint, the larger one wins
// the rank (delivers more relief on the dominant dimension).
func TestScore_BiggerFootprintScoresHigher(t *testing.T) {
	small := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint = rebalance.Footprint{CPU: 0.10, Memory: 0.05}
	})
	large := candidate(func(c *rebalance.MoveCandidate) {
		c.Footprint = rebalance.Footprint{CPU: 0.40, Memory: 0.05}
	})
	sSmall := rebalance.Score(small)
	sLarge := rebalance.Score(large)
	if sLarge <= sSmall {
		t.Errorf("large-footprint score (%v) should be > small-footprint score (%v)", sLarge, sSmall)
	}
}

// TestPostMovePressure_SubtractsFromSrcAddsToDst — sanity that the
// arithmetic the cycle uses for dst_cap / relief_floor gates matches
// the footprint.
func TestPostMovePressure_SubtractsFromSrcAddsToDst(t *testing.T) {
	c := candidate(func(c *rebalance.MoveCandidate) {
		c.SrcPressure = rebalance.Snapshot{CPU: 0.9, Memory: 0.5}
		c.DstPressure = rebalance.Snapshot{CPU: 0.2, Memory: 0.1}
		c.Footprint = rebalance.Footprint{CPU: 0.3, Memory: 0.2}
	})
	postSrc, postDst := rebalance.PostMovePressure(c)
	if math.Abs(postSrc.CPU-0.6) > 1e-9 {
		t.Errorf("postSrc.CPU = %v, want 0.6", postSrc.CPU)
	}
	if math.Abs(postSrc.Memory-0.3) > 1e-9 {
		t.Errorf("postSrc.Memory = %v, want 0.3", postSrc.Memory)
	}
	if math.Abs(postDst.CPU-0.5) > 1e-9 {
		t.Errorf("postDst.CPU = %v, want 0.5", postDst.CPU)
	}
	if math.Abs(postDst.Memory-0.3) > 1e-9 {
		t.Errorf("postDst.Memory = %v, want 0.3", postDst.Memory)
	}
}

// TestPostMovePressure_ClampsAtZero — when a footprint exceeds the
// pre-move snapshot, post stays at 0 rather than going negative.
// Guards the gates from spurious "relief = 1.something" math.
func TestPostMovePressure_ClampsAtZero(t *testing.T) {
	c := candidate(func(c *rebalance.MoveCandidate) {
		c.SrcPressure = rebalance.Snapshot{CPU: 0.1, Memory: 0.05}
		c.Footprint = rebalance.Footprint{CPU: 0.5, Memory: 0.5}
	})
	postSrc, _ := rebalance.PostMovePressure(c)
	if postSrc.CPU != 0 || postSrc.Memory != 0 {
		t.Errorf("post src not clamped to zero: %+v", postSrc)
	}
}

// TestHardFilter_OrderingAndReasons — explicit table over each gate.
// Order matters: resource_limits must come before anti_affinity per
// the package's HardFilter ordering. (The order is observable
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
			if got := rebalance.HardFilter(c); got != tc.want {
				t.Errorf("HardFilter = %q, want %q", got, tc.want)
			}
		})
	}
}
