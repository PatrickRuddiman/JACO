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
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MaxSetNameLen is nftables' identifier length limit.
const MaxSetNameLen = 63

// Subnet is the per-(deployment, network) CIDR the ruleset locks down. One
// nftables `set` is emitted per Subnet; the forward chain matches packets
// whose saddr + daddr both belong to the same set.
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
// rendering.
func Render(in RuleInput) string {
	subnets := append([]Subnet(nil), in.Subnets...)
	sort.Slice(subnets, func(i, j int) bool {
		if subnets[i].Deployment != subnets[j].Deployment {
			return subnets[i].Deployment < subnets[j].Deployment
		}
		return subnets[i].Network < subnets[j].Network
	})

	// Group CIDRs by nftables set name: with per-host /24s (issue #28),
	// several CIDRs share one (deployment, network) set, so cross-host
	// intra-deployment traffic (saddr in host-A's /24, daddr in host-B's)
	// matches @set on both sides.
	var setOrder []string
	cidrsBySet := map[string][]string{}
	for _, s := range subnets {
		name := SetName(s.Deployment, s.Network)
		if _, ok := cidrsBySet[name]; !ok {
			setOrder = append(setOrder, name)
		}
		cidrsBySet[name] = append(cidrsBySet[name], s.CIDR)
	}
	for name := range cidrsBySet {
		sort.Strings(cidrsBySet[name])
	}

	var b strings.Builder
	fmt.Fprintln(&b, "table inet jaco {")

	// Named sets — one per (deployment, network), holding every host's /24.
	for _, name := range setOrder {
		fmt.Fprintf(&b, "    set %s {\n", name)
		fmt.Fprintf(&b, "        type ipv4_addr\n")
		fmt.Fprintf(&b, "        flags interval\n")
		fmt.Fprintf(&b, "        elements = { %s }\n", strings.Join(cidrsBySet[name], ", "))
		fmt.Fprintf(&b, "    }\n\n")
	}

	// forward chain — east-west isolation.
	fmt.Fprintln(&b, "    chain forward {")
	fmt.Fprintln(&b, "        type filter hook forward priority 0; policy drop;")
	fmt.Fprintln(&b, "        ct state established,related accept")
	for _, name := range setOrder {
		fmt.Fprintf(&b, "        ip saddr @%s ip daddr @%s accept\n", name, name)
	}
	fmt.Fprintln(&b, "        drop")
	fmt.Fprintln(&b, "    }")
	fmt.Fprintln(&b, "")

	// input chain — north-south admission.
	fmt.Fprintln(&b, "    chain input {")
	fmt.Fprintln(&b, "        type filter hook input priority 0; policy drop;")
	fmt.Fprintln(&b, "        iif lo accept")
	fmt.Fprintln(&b, "        ct state established,related accept")
	wgPort := in.WGPort
	if wgPort == 0 {
		wgPort = 51820
	}
	fmt.Fprintf(&b, "        udp dport %d accept\n", wgPort)
	fmt.Fprintln(&b, "        iifname \"wg-jaco\" accept")
	fmt.Fprintln(&b, "        iifname \"jaco-*\" udp dport 53 accept")
	grpcPort := in.GrpcPort
	if grpcPort == 0 {
		grpcPort = 7000
	}
	fmt.Fprintf(&b, "        tcp dport %d accept\n", grpcPort)
	if len(in.IngressPorts) > 0 {
		ports := append([]int(nil), in.IngressPorts...)
		sort.Ints(ports)
		ps := make([]string, 0, len(ports))
		for _, p := range ports {
			ps = append(ps, fmt.Sprintf("%d", p))
		}
		if len(ports) == 1 {
			fmt.Fprintf(&b, "        tcp dport %s accept\n", ps[0])
		} else {
			fmt.Fprintf(&b, "        tcp dport { %s } accept\n", strings.Join(ps, ", "))
		}
	}
	fmt.Fprintln(&b, "        drop")
	fmt.Fprintln(&b, "    }")
	fmt.Fprintln(&b, "")

	// output chain — unrestricted.
	fmt.Fprintln(&b, "    chain output {")
	fmt.Fprintln(&b, "        type filter hook output priority 0; policy accept;")
	fmt.Fprintln(&b, "    }")
	fmt.Fprintln(&b, "}")
	return b.String()
}

// SetName builds the nftables set identifier for (deployment, network).
// Sanitizes the input to `[a-zA-Z0-9_]` (nftables identifiers can't contain
// dashes / dots) and hashes when the joined identifier would exceed
// MaxSetNameLen. The hash form preserves the human-readable prefix so
// debugging-from-rules is still possible.
func SetName(deployment, network string) string {
	if network == "" {
		network = "_default"
	}
	full := fmt.Sprintf("dep_net_%s_%s", sanitize(deployment), sanitize(network))
	if len(full) <= MaxSetNameLen {
		return full
	}
	// Hash form: dep_net_<sha1>_<truncated-prefix> — total length still <= 63.
	sum := sha1.Sum([]byte(deployment + "/" + network))
	prefix := full[:len("dep_net_")+8]
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(sum[:])[:8])
}

var sanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func sanitize(s string) string {
	return sanitizeRE.ReplaceAllString(s, "_")
}
