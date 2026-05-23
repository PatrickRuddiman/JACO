package pull_test

import (
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/runtime/pull"
)

// TestSystemClock_NowMatchesReal — SystemClock.Now is bracketed by
// real time.Now()s.
func TestSystemClock_NowMatchesReal(t *testing.T) {
	c := pull.SystemClock()
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after.Add(time.Second)) {
		t.Errorf("Now() = %v, not bracketed by %v..%v", got, before, after)
	}
}

// TestSystemClock_AfterFiresAtRequestedInterval — After fires within
// ~10ms of the requested delay (allow generous slack so we don't
// flake under load).
func TestSystemClock_AfterFiresAtRequestedInterval(t *testing.T) {
	c := pull.SystemClock()
	ch := c.After(10 * time.Millisecond)
	start := time.Now()
	<-ch
	elapsed := time.Since(start)
	if elapsed < 5*time.Millisecond {
		t.Errorf("After fired in %v, expected ~10ms or more", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("After fired in %v, expected ~10ms (much slower than expected — flaky?)", elapsed)
	}
}
