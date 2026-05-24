package firewall

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// snatExemptRule is the iptables rule-spec that exempts intra-pool container
// traffic from Docker's per-bridge MASQUERADE, so cross-host container source
// IPs survive when routed over jaco0 (issue #28). Returned as args so the
// caller can pass them to both `iptables -C` and `-I`.
func snatExemptRule(pool string) []string {
	return []string{"-s", pool, "-d", pool, "-j", "RETURN"}
}

// EnsureSNATExempt makes sure a RETURN for intra-pool traffic sits at the TOP
// of the nat POSTROUTING chain Docker masquerades in. The RETURN must be
// ABOVE Docker's per-bridge MASQUERADE, or cross-host container traffic
// routed over jaco0 gets SNAT'd to the host IP (issue #28). Docker re-prepends
// its MASQUERADE rule on every network create — which would push a
// previously-top RETURN down — and a plain `iptables -C` existence check
// passes even when the rule has drifted below Docker's, so it would never get
// re-topped. To guarantee placement, this deletes every copy and re-inserts
// at position 1 unless the rule is already first. Re-asserted every reconcile
// tick (it lives outside `table inet jaco` / its SelfTest).
func EnsureSNATExempt(ctx context.Context, pool string) error {
	rule := snatExemptRule(pool)
	if firstPostroutingIsExempt(ctx, pool) {
		return nil // already at the top — nothing to do
	}
	// Drop any drifted/duplicate copies, then put exactly one at the top.
	del := append([]string{"-t", "nat", "-D", "POSTROUTING"}, rule...)
	for i := 0; i < 16; i++ {
		if err := exec.CommandContext(ctx, "iptables", del...).Run(); err != nil {
			break // no (more) copies to delete
		}
	}
	insert := append([]string{"-t", "nat", "-I", "POSTROUTING", "1"}, rule...)
	if out, err := exec.CommandContext(ctx, "iptables", insert...).CombinedOutput(); err != nil {
		return fmt.Errorf("iptables insert SNAT exempt %s: %w (output: %s)", pool, err, string(out))
	}
	return nil
}

// firstPostroutingIsExempt reports whether the first rule of nat POSTROUTING
// is already our intra-pool RETURN (so we avoid churning it every tick).
func firstPostroutingIsExempt(ctx context.Context, pool string) bool {
	out, err := exec.CommandContext(ctx, "iptables", "-t", "nat", "-S", "POSTROUTING").CombinedOutput()
	if err != nil {
		return false
	}
	want := fmt.Sprintf("-A POSTROUTING -s %s -d %s -j RETURN", pool, pool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-A POSTROUTING") {
			continue // skip the `-P`/policy line(s)
		}
		return line == want // first appended rule must be our RETURN
	}
	return false
}
