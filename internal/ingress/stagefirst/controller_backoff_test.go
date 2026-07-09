package stagefirst

import (
	"testing"
	"time"
)

// TestProdBackoffFor verifies the unexported helper directly: 15m→30m→1h cap.
// Issue #189.
func TestProdBackoffFor(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, ProdBackoffBase},    // n≤0 → base
		{1, 15 * time.Minute},  // fail1 = 15m
		{2, 30 * time.Minute},  // fail2 = 30m
		{3, 60 * time.Minute},  // fail3 = 1h (cap)
		{4, 60 * time.Minute},  // fail4 = still capped
		{100, ProdBackoffMax},  // large n = still capped
	}
	for _, c := range cases {
		if got := prodBackoffFor(c.n); got != c.want {
			t.Errorf("prodBackoffFor(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
