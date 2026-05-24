package firewall

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// meshSubnet mirrors wgmesh.MeshNetwork — the WG mesh /24. Host-originated
// overlay traffic (the embedded ingress proxying to a replica on another host)
// is sourced from this node's mesh IP, so the destination host must admit
// mesh→pool just like container→container pool→pool (issue #28). Kept as a
// local const to avoid a firewall→wgmesh import; both must stay in sync.
const meshSubnet = "10.99.0.0/24"

// overlayAcceptRule is the iptables rule-spec (sans table/chain/op) for one
// overlay source→dest ACCEPT. Returned as args so the caller can pass it to
// `iptables -D` and `-I` alike.
func overlayAcceptRule(src, dst string) []string {
	return []string{"-s", src, "-d", dst, "-j", "ACCEPT"}
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
// Two overlay sources legitimately reach a container across hosts: another
// container (pool→pool, east-west) and a host's ingress sourced from the WG
// mesh IP (mesh→pool, north-south — the only way a node with no local replica
// can proxy). We pin both ACCEPTs to the TOP of each chain (raw PREROUTING and
// the Docker-sanctioned DOCKER-USER chain). Docker re-adds per-container drops
// as containers come and go, and a plain `-C` check passes even when our
// ACCEPTs have drifted below a later-added drop — so this re-pins them whenever
// the top of the chain no longer matches, rather than trusting a one-shot
// insert.
func EnsureOverlayExempt(ctx context.Context, pool string) error {
	specs := [][2]string{{pool, pool}, {meshSubnet, pool}}
	if err := ensureTopAccepts(ctx, []string{"-t", "raw"}, "PREROUTING", specs); err != nil {
		return fmt.Errorf("raw PREROUTING overlay exempt: %w", err)
	}
	if err := ensureTopAccepts(ctx, nil, "DOCKER-USER", specs); err != nil {
		return fmt.Errorf("DOCKER-USER overlay exempt: %w", err)
	}
	return nil
}

// ensureTopAccepts guarantees the given src→dst ACCEPTs occupy the top of the
// chain, in order. No-op when they already do (avoids per-tick churn); else it
// deletes any stray copies and re-inserts all of them at the top.
func ensureTopAccepts(ctx context.Context, tableArgs []string, chain string, specs [][2]string) error {
	wants := make([]string, len(specs))
	for i, sp := range specs {
		wants[i] = overlayWant(chain, sp[0], sp[1])
	}
	if out, err := exec.CommandContext(ctx, "iptables", concatArgs(tableArgs, []string{"-S", chain}, nil)...).CombinedOutput(); err == nil {
		if topAppendedRulesMatch(string(out), chain, wants) {
			return nil // already pinned at the top — nothing to do
		}
	}
	// Drop any drifted/duplicate copies of each rule.
	for _, sp := range specs {
		del := concatArgs(tableArgs, []string{"-D", chain}, overlayAcceptRule(sp[0], sp[1]))
		for i := 0; i < 16; i++ {
			if err := exec.CommandContext(ctx, "iptables", del...).Run(); err != nil {
				break // no (more) copies to delete
			}
		}
	}
	// Insert in reverse so specs[0] lands topmost.
	for i := len(specs) - 1; i >= 0; i-- {
		insert := concatArgs(tableArgs, []string{"-I", chain, "1"}, overlayAcceptRule(specs[i][0], specs[i][1]))
		if out, err := exec.CommandContext(ctx, "iptables", insert...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables insert %s overlay accept: %w (output: %s)", chain, err, string(out))
		}
	}
	return nil
}

// overlayWant is the `iptables -S` rendering of one src→dst ACCEPT once
// appended to the chain.
func overlayWant(chain, src, dst string) string {
	return fmt.Sprintf("-A %s -s %s -d %s -j ACCEPT", chain, src, dst)
}

// topAppendedRulesMatch reports whether the first len(wants) `-A <chain>` lines
// in `iptables -S` output equal wants, in order (policy `-P` lines and other
// chains are skipped). Pure so the drift check is unit-testable.
func topAppendedRulesMatch(savedOutput, chain string, wants []string) bool {
	prefix := "-A " + chain
	var appended []string
	for _, line := range strings.Split(savedOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			appended = append(appended, line)
		}
	}
	if len(appended) < len(wants) {
		return false
	}
	for i, w := range wants {
		if appended[i] != w {
			return false
		}
	}
	return true
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
