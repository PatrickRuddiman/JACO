package rebalance

import (
	"math"
	"time"
)

// EWMA is a continuous-time exponentially-weighted moving average with
// a configurable half-life window. The composite pressure score is
// sampled once per cycle (default 30s) and fed into per-node EWMAs
// (default window 5m, per ADR 0002 §"Signals") so transient spikes
// don't trigger moves — only sustained drift does.
//
// "Continuous-time" means the decay factor between samples is derived
// from the actual elapsed wall time, not from the sample count. This
// keeps the half-life stable when the cycle runs slower or faster than
// CycleInterval (e.g. under load, when GC stalls a tick, or when a
// test drives Update with a non-uniform clock).
//
// Zero value is valid: Value returns 0 before the first Update, and the
// first Update seeds the average to that sample exactly (no implicit
// "warm-up to half" period).
type EWMA struct {
	window  time.Duration
	value   float64
	hasData bool
	lastAt  time.Time
}

// NewEWMA constructs an EWMA with the given window. A zero or negative
// window is normalised to 1 nanosecond so the next Update collapses to
// "take the new sample as the value" — degenerate but safe; callers
// who pass DefaultConfig().CycleInterval*10 (5m) get the documented
// behaviour.
func NewEWMA(window time.Duration) *EWMA {
	if window <= 0 {
		window = time.Nanosecond
	}
	return &EWMA{window: window}
}

// Update folds sample (taken at now) into the moving average. The
// first call seeds the average; subsequent calls apply
//
//	alpha = 1 - exp(-dt / window)
//	value = value + alpha * (sample - value)
//
// where dt is now − lastUpdate. dt ≤ 0 (clock jitter, repeated
// samples) is treated as zero elapsed time: the new sample is folded
// in with alpha = 0, i.e. the EWMA stays put. (Strictly clamping
// non-positive dt to zero is correct: we never want a clock jump to
// suddenly rewind the average.)
func (e *EWMA) Update(now time.Time, sample float64) {
	if !e.hasData {
		e.value = sample
		e.lastAt = now
		e.hasData = true
		return
	}
	dt := now.Sub(e.lastAt)
	if dt < 0 {
		dt = 0
	}
	alpha := 1 - math.Exp(-float64(dt)/float64(e.window))
	e.value += alpha * (sample - e.value)
	e.lastAt = now
}

// Value returns the current moving average. Zero before the first
// Update.
func (e *EWMA) Value() float64 {
	return e.value
}

// HasData reports whether at least one sample has been folded in.
// Useful for callers that want to distinguish "no data yet" (skip the
// node this cycle) from "data, value = 0".
func (e *EWMA) HasData() bool {
	return e.hasData
}
