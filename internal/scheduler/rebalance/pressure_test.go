package rebalance_test

import (
	"math"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
)

// TestComposite_Math — Composite returns max(CPU, Memory) and
// Dominant picks the larger dimension. Edge cases that used to exist
// (disk-io, count/cap) were removed when the rebalancer was scoped
// down to a simple orchestrator.
func TestComposite_Math(t *testing.T) {
	cases := []struct {
		name string
		snap rebalance.Snapshot
		want float64
		dom  rebalance.Dimension
	}{
		{
			name: "all zero",
			snap: rebalance.Snapshot{},
			want: 0,
			dom:  rebalance.DimCPU,
		},
		{
			name: "cpu dominates",
			snap: rebalance.Snapshot{CPU: 0.9, Memory: 0.4},
			want: 0.9,
			dom:  rebalance.DimCPU,
		},
		{
			name: "memory dominates",
			snap: rebalance.Snapshot{CPU: 0.3, Memory: 0.95},
			want: 0.95,
			dom:  rebalance.DimMemory,
		},
		{
			name: "tie breaks toward CPU",
			snap: rebalance.Snapshot{CPU: 0.5, Memory: 0.5},
			want: 0.5,
			dom:  rebalance.DimCPU,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rebalance.Composite(c.snap); math.Abs(got-c.want) > 1e-9 {
				t.Errorf("Composite = %v, want %v", got, c.want)
			}
			if got := rebalance.Dominant(c.snap); got != c.dom {
				t.Errorf("Dominant = %v, want %v", got, c.dom)
			}
		})
	}
}

// TestEWMA_FirstSampleSeedsValue — first Update seeds the average to
// that sample exactly. No "warm up to half" behaviour.
func TestEWMA_FirstSampleSeedsValue(t *testing.T) {
	e := rebalance.NewEWMA(5 * time.Minute)
	if e.HasData() {
		t.Fatalf("HasData = true before any Update")
	}
	if v := e.Value(); v != 0 {
		t.Fatalf("zero-value Value = %v, want 0", v)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.Update(now, 0.7)
	if !e.HasData() {
		t.Errorf("HasData = false after first Update")
	}
	if v := e.Value(); v != 0.7 {
		t.Errorf("first-sample Value = %v, want 0.7", v)
	}
}

// TestEWMA_Decay_HalfLifeMatchesWindow — after one window-worth of
// elapsed time, a step input from 0 to 1 should sit close to
// 1 - exp(-1) ≈ 0.632. Verifies the alpha formula matches the
// documented "5-minute window" semantics.
func TestEWMA_Decay_HalfLifeMatchesWindow(t *testing.T) {
	window := 5 * time.Minute
	e := rebalance.NewEWMA(window)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.Update(now, 0)
	now = now.Add(window)
	e.Update(now, 1)
	want := 1 - math.Exp(-1) // ≈ 0.6321
	if got := e.Value(); math.Abs(got-want) > 1e-6 {
		t.Errorf("Value after one window = %v, want ≈ %v", got, want)
	}
}

// TestEWMA_Convergence_RepeatedSamplesApproachInput — feeding the
// same sample for many windows converges toward it. The cycle-loop
// uses this: a node that stays at 0.9 for several minutes should
// have an EWMA at 0.9 ± tolerance.
func TestEWMA_Convergence_RepeatedSamplesApproachInput(t *testing.T) {
	e := rebalance.NewEWMA(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.Update(now, 0)
	// Feed 0.9 once per 30s for an hour (120 samples).
	for i := 0; i < 120; i++ {
		now = now.Add(30 * time.Second)
		e.Update(now, 0.9)
	}
	if got := e.Value(); math.Abs(got-0.9) > 0.01 {
		t.Errorf("Value after 1h sustained 0.9 = %v, want ≈ 0.9", got)
	}
}

// TestEWMA_SpikeDamped — a single spike on an otherwise calm series
// should NOT drag the average anywhere near the spike. This is the
// behaviour ADR 0002 §"Signals" buys: hysteresis against transient
// noise.
func TestEWMA_SpikeDamped(t *testing.T) {
	e := rebalance.NewEWMA(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.Update(now, 0.2)
	// Hold at 0.2 for 4 minutes, then a single 1.0 spike.
	for i := 0; i < 8; i++ {
		now = now.Add(30 * time.Second)
		e.Update(now, 0.2)
	}
	now = now.Add(30 * time.Second)
	e.Update(now, 1.0)
	// One 30s sample with window 5m → alpha ≈ 1 - exp(-0.1) ≈ 0.0952.
	// post = 0.2 + 0.0952 * (1 - 0.2) ≈ 0.276 — nowhere near 1.0.
	if got := e.Value(); got > 0.35 {
		t.Errorf("Value after single spike = %v, want < 0.35 (damped)", got)
	}
}

// TestEWMA_BackwardsClock — a non-monotonic clock (sample at an
// earlier time than the previous) is treated as dt = 0, leaving the
// average unchanged. Defends the leader-local state from accidental
// rewinds.
func TestEWMA_BackwardsClock(t *testing.T) {
	e := rebalance.NewEWMA(5 * time.Minute)
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	e.Update(now, 0.4)
	before := e.Value()
	// Rewind 1 minute, sample 1.0 — should not budge.
	e.Update(now.Add(-1*time.Minute), 1.0)
	if got := e.Value(); got != before {
		t.Errorf("backwards-clock Update changed Value from %v to %v", before, got)
	}
}
