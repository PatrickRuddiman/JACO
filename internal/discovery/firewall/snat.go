package firewall

import (
	"context"
	"fmt"
	"os/exec"
)

// snatExemptRule is the iptables rule-spec that exempts intra-pool container
// traffic from Docker's per-bridge MASQUERADE, so cross-host container source
// IPs survive when routed over jaco0 (issue #28). Returned as args so the
// caller can pass them to both `iptables -C` and `-I`.
func snatExemptRule(pool string) []string {
	return []string{"-s", pool, "-d", pool, "-j", "RETURN"}
}

// EnsureSNATExempt makes sure a RETURN for intra-pool traffic sits at the top
// of the nat POSTROUTING chain Docker masquerades in. Idempotent: it checks
// with `iptables -C` and inserts at position 1 only when the rule is missing,
// so it survives Docker re-writing the chain on network create/restart.
// Lives outside `table inet jaco` (and its SelfTest), so the reconciler
// re-asserts it on every tick.
func EnsureSNATExempt(ctx context.Context, pool string) error {
	rule := snatExemptRule(pool)
	check := append([]string{"-t", "nat", "-C", "POSTROUTING"}, rule...)
	if err := exec.CommandContext(ctx, "iptables", check...).Run(); err == nil {
		return nil // already present
	}
	insert := append([]string{"-t", "nat", "-I", "POSTROUTING", "1"}, rule...)
	if out, err := exec.CommandContext(ctx, "iptables", insert...).CombinedOutput(); err != nil {
		return fmt.Errorf("iptables insert SNAT exempt %s: %w (output: %s)", pool, err, string(out))
	}
	return nil
}
