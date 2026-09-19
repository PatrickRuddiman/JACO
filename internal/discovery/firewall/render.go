// Package firewall renders the JACO nftables ruleset (table inet jaco),
// applies it via `nft -f`, and runs a post-apply self-test against
// `nft -j list table inet jaco` so the daemon refuses to signal
// `sd_notify(READY=1)` when the host's actual ruleset doesn't match what
// JACO expects.
//
// Render is pure-Go — testable via golden files. Apply / SelfTest call out
// to the `nft` binary; those tests are deferred to the CI rig (task 31).
package firewall

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"sort"
	"strings"
)

// MaxSetNameLen is nftables' identifier length limit.
const MaxSetNameLen = 63

// Subnet is a host's CIDR for an exact (deployment, network) scope. All hosts
// in that scope share one nftables set; both packet addresses must match it.
type Subnet struct {
	Deployment string
	Network    string
	CIDR       string
}

// RuleInput is everything Render needs. Caller sources Subnets from
// state.Subnets and the port set from cluster config.
type RuleInput struct {
	Subnets      []Subnet
	WGPort       int   // WireGuard UDP listen port (default 51820)
	GrpcPort     int   // JACO control-plane TCP port (default 7000)
	IngressPorts []int // Caddy reverse-proxy TCP ports (typically 80, 443)
}

// Render emits the full `table inet jaco` ruleset as a single string. The
// caller can pipe it through `nft -f -` or write it to a temp file.
// Deterministic — Subnets are sorted by (deployment, network) before
// rendering. A set-name collision returns an error and no ruleset.
func Render(in RuleInput) (string, error) {
	return render(in, SetName)
}

func render(in RuleInput, setName func(string, string) string) (string, error) {
	sets, err := groupSubnets(in.Subnets, setName)
	if err != nil {
		return "", err
	}

	// allCIDRs is the union of every JACO subnet — the "pool". It scopes the
	// cross-network isolation drop so JACO never touches traffic outside its
	// own subnets (the operator's other networks/routing are left alone).
	var allCIDRs []string
	for _, set := range sets {
		allCIDRs = append(allCIDRs, set.cidrs...)
	}
	sort.Strings(allCIDRs)

	var b strings.Builder

	// Atomic replace: `nft -f` APPENDS to an existing chain rather than
	// replacing it, so re-applying a `table inet jaco { ... }` block on every
	// reconcile stacks another full generation of forward rules onto the live
	// chain. Earlier generations' `@jaco_pool ... drop` then sits AHEAD of a
	// later-deployed stack's per-scope accept, shadowing it — which silently
	// breaks cross-host traffic for every deployment except the first one
	// applied (same-host traffic L2-switches within one bridge and never hits
	// the forward hook, so it stays reachable, which is what made this subtle).
	//
	// `add table` creates the table if absent (no-op if present) so the
	// following `delete table` can't fail on a cold host; `delete` then drops
	// the entire prior generation. Because `nft -f` runs the whole file as one
	// atomic transaction, the table is recreated from scratch on every apply —
	// the chains never accumulate. SNAT/overlay exemptions live in Docker's own
	// nat/raw tables (re-asserted each tick), so flushing inet jaco is safe.
	fmt.Fprintln(&b, "add table inet jaco")
	fmt.Fprintln(&b, "delete table inet jaco")
	fmt.Fprintln(&b, "table inet jaco {")

	// Named sets — one per (deployment, network), holding every host's /24.
	for _, set := range sets {
		fmt.Fprintf(&b, "    set %s {\n", set.name)
		fmt.Fprintf(&b, "        type ipv4_addr\n")
		fmt.Fprintf(&b, "        flags interval\n")
		fmt.Fprintf(&b, "        elements = { %s }\n", strings.Join(set.cidrs, ", "))
		fmt.Fprintf(&b, "    }\n\n")
	}
	// jaco_pool — union of all JACO subnets; the isolation drop is scoped to it.
	if len(allCIDRs) > 0 {
		fmt.Fprintf(&b, "    set jaco_pool {\n")
		fmt.Fprintf(&b, "        type ipv4_addr\n")
		fmt.Fprintf(&b, "        flags interval\n")
		fmt.Fprintf(&b, "        elements = { %s }\n", strings.Join(allCIDRs, ", "))
		fmt.Fprintf(&b, "    }\n\n")
	}

	// forward chain — east-west isolation of JACO's OWN container subnets.
	// Policy ACCEPT: JACO must never drop traffic it doesn't own (the
	// operator's other docker networks, host routing, VPN/VNet forwarding,
	// etc.). The ONLY thing dropped is traffic BETWEEN two JACO subnets in
	// DIFFERENT (deployment, network) scopes — same-scope is allowed (incl.
	// cross-host, via the per-scope set), and anything outside jaco_pool is
	// untouched by the accept policy.
	fmt.Fprintln(&b, "    chain forward {")
	fmt.Fprintln(&b, "        type filter hook forward priority 0; policy accept;")
	// Conntrack entries created before a reload must not bypass scope checks.
	for _, set := range sets {
		fmt.Fprintf(&b, "        ip saddr @%s ip daddr @%s accept\n", set.name, set.name)
	}
	if len(allCIDRs) > 0 {
		fmt.Fprintln(&b, "        ip saddr @jaco_pool ip daddr @jaco_pool drop")
	}
	fmt.Fprintln(&b, "    }")
	fmt.Fprintln(&b, "")

	// input chain — JACO does NOT police host ingress. The operator's access
	// (SSH on whatever port, a VPN, a VNet, Tailscale, their own host firewall)
	// is their domain; a policy-drop input chain would silently lock them out
	// and trespass on choices JACO can't know. Policy ACCEPT, no rules — the
	// chain exists only so the table's shape is stable for drift detection.
	fmt.Fprintln(&b, "    chain input {")
	fmt.Fprintln(&b, "        type filter hook input priority 0; policy accept;")
	fmt.Fprintln(&b, "    }")
	fmt.Fprintln(&b, "")

	// output chain — unrestricted.
	fmt.Fprintln(&b, "    chain output {")
	fmt.Fprintln(&b, "        type filter hook output priority 0; policy accept;")
	fmt.Fprintln(&b, "    }")
	fmt.Fprintln(&b, "}")
	return b.String(), nil
}

// SetName builds the nftables set identifier for (deployment, network).
// Length-prefixed original fields preserve punctuation and tuple boundaries.
// The full SHA-256 digest fits in 60 identifier-safe characters with the prefix.
func SetName(deployment, network string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(deployment), deployment, len(network), network)))
	return "dep_net_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
}

type scope struct {
	deployment string
	network    string
}

type scopeSet struct {
	name  string
	cidrs []string
}

func groupSubnets(subnets []Subnet, setName func(string, string) string) ([]scopeSet, error) {
	cidrsByScope := map[scope][]string{}
	var scopes []scope
	for _, subnet := range subnets {
		key := scope{subnet.Deployment, subnet.Network}
		if _, exists := cidrsByScope[key]; !exists {
			scopes = append(scopes, key)
		}
		cidrsByScope[key] = append(cidrsByScope[key], subnet.CIDR)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].deployment != scopes[j].deployment {
			return scopes[i].deployment < scopes[j].deployment
		}
		return scopes[i].network < scopes[j].network
	})

	owners := map[string]scope{}
	sets := make([]scopeSet, 0, len(scopes))
	for _, key := range scopes {
		name := setName(key.deployment, key.network)
		if previous, exists := owners[name]; exists {
			return nil, fmt.Errorf("nftables set name collision %q between scopes (%q, %q) and (%q, %q)",
				name, previous.deployment, previous.network, key.deployment, key.network)
		}
		owners[name] = key
		cidrs := cidrsByScope[key]
		sort.Strings(cidrs)
		sets = append(sets, scopeSet{name: name, cidrs: cidrs})
	}
	return sets, nil
}
