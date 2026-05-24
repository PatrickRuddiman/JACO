package firewall

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// overlayAcceptRule is the iptables rule-spec (sans table/chain/op) that
// matches intra-pool overlay traffic: packets whose source AND destination are
// both inside the IPAM /16. Cross-host container traffic arriving over the WG
// mesh (jaco0) is always pool→pool, so this is the exemption key for Docker's
// container-isolation drops (issue #28). Returned as args so the caller can
// pass them to `iptables -C`, `-D`, and `-I` alike.
func overlayAcceptRule(pool string) []string {
	return []string{"-s", pool, "-d", pool, "-j", "ACCEPT"}
}

// EnsureOverlayExempt clears the two Docker (28+) firewall drops that silently
// kill cross-host container traffic arriving over the WG mesh. Docker treats
// any non-bridge ingress interface (jaco0) as untrusted and drops packets
// destined to a container IP in two hooks, BOTH of which fire before delivery
// to the bridge — so the request reaches the destination host's jaco0 but
// never the container, with no reply (issue #28):
//
//   - raw PREROUTING (priority -300): a per-container
//     `-d <container-ip> ! -i <bridge> -j DROP` ("direct routing" hardening).
//   - filter FORWARD → DOCKER: `! -i <bridge> -o <bridge> -j DROP`
//     (inter-network isolation).
//
// We punch a pool→pool ACCEPT to the TOP of each chain (raw PREROUTING and the
// Docker-sanctioned DOCKER-USER chain, which is traversed before the isolation
// drop). Docker re-adds per-container drops as containers come and go, and a
// plain `-C` existence check passes even when our ACCEPT has drifted below a
// later-added drop — so, exactly like EnsureSNATExempt, this force-tops the
// rule every reconcile tick rather than trusting a one-shot insert.
func EnsureOverlayExempt(ctx context.Context, pool string) error {
	// raw PREROUTING — clears the per-container direct-routing DROP.
	if err := ensureTopAccept(ctx, []string{"-t", "raw"}, "PREROUTING", pool); err != nil {
		return fmt.Errorf("raw PREROUTING overlay exempt: %w", err)
	}
	// DOCKER-USER — runs first in FORWARD, ahead of Docker's isolation DROP.
	if err := ensureTopAccept(ctx, nil, "DOCKER-USER", pool); err != nil {
		return fmt.Errorf("DOCKER-USER overlay exempt: %w", err)
	}
	return nil
}

// ensureTopAccept guarantees exactly one pool→pool ACCEPT sits at the top of
// the named chain (tableArgs is e.g. ["-t","raw"], or nil for the default
// filter table). Deletes every drifted/duplicate copy, then inserts one at
// position 1 — unless it is already first, in which case it does nothing.
func ensureTopAccept(ctx context.Context, tableArgs []string, chain, pool string) error {
	if firstRuleIsPoolAccept(ctx, tableArgs, chain, pool) {
		return nil // already at the top — nothing to do
	}
	rule := overlayAcceptRule(pool)
	del := concatArgs(tableArgs, []string{"-D", chain}, rule)
	for i := 0; i < 16; i++ {
		if err := exec.CommandContext(ctx, "iptables", del...).Run(); err != nil {
			break // no (more) copies to delete
		}
	}
	insert := concatArgs(tableArgs, []string{"-I", chain, "1"}, rule)
	if out, err := exec.CommandContext(ctx, "iptables", insert...).CombinedOutput(); err != nil {
		return fmt.Errorf("iptables insert %s overlay accept: %w (output: %s)", chain, err, string(out))
	}
	return nil
}

// firstRuleIsPoolAccept reports whether the first appended rule of the chain is
// already our pool→pool ACCEPT (so we avoid churning it every tick).
func firstRuleIsPoolAccept(ctx context.Context, tableArgs []string, chain, pool string) bool {
	args := concatArgs(tableArgs, []string{"-S", chain}, nil)
	out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
	if err != nil {
		return false
	}
	return firstAppendedRuleIs(string(out), chain, overlayWant(chain, pool))
}

// overlayWant is the `iptables -S` rendering our pool→pool ACCEPT takes once
// appended to the chain — the exact string firstAppendedRuleIs compares against.
func overlayWant(chain, pool string) string {
	return fmt.Sprintf("-A %s -s %s -d %s -j ACCEPT", chain, pool, pool)
}

// firstAppendedRuleIs reports whether the first `-A <chain>` line in `iptables
// -S` output equals want (policy `-P` lines and other chains are skipped). Pure
// so the drift-detection logic is unit-testable without shelling out.
func firstAppendedRuleIs(savedOutput, chain, want string) bool {
	prefix := "-A " + chain
	for _, line := range strings.Split(savedOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue // skip the `-P`/policy line(s) and unrelated chains
		}
		return line == want // first appended rule must be our ACCEPT
	}
	return false
}

// concatArgs joins arg groups into a fresh slice (no aliasing — each call to a
// builder must own its backing array so repeated exec.Command calls don't
// clobber each other).
func concatArgs(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
