package firewall_test

import (
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
)

func TestRender_DistinctScopesStayIsolated(t *testing.T) {
	cases := []struct {
		name                  string
		deploymentA, networkA string
		deploymentB, networkB string
	}{
		{"network punctuation", "demo", "private-net", "demo", "private.net"},
		{"network underscore", "demo", "private-net", "demo", "private_net"},
		{"tuple boundary", "a-b", "c", "a", "b-c"},
		{"empty network", "demo", "", "demo", "_default"},
		{"long names", strings.Repeat("a", 80), "private-net", strings.Repeat("a", 80), "private.net"},
		{"long tuple encoding", strings.Repeat("a", 80) + "/b", "c", strings.Repeat("a", 80), "b/c"},
		{"length delimiters", "a:1", "b", "a", "1:b"},
		{"embedded delimiter", "a\x00b", "c", "a", "b\x00c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderRules(t, firewall.RuleInput{
				Subnets: []firewall.Subnet{
					{Deployment: tc.deploymentA, Network: tc.networkA, CIDR: "10.244.1.0/24"},
					{Deployment: tc.deploymentB, Network: tc.networkB, CIDR: "10.244.2.0/24"},
					{Deployment: tc.deploymentA, Network: tc.networkA, CIDR: "10.244.3.0/24"},
				},
			})
			for _, pair := range [][2]string{
				{"10.244.1.2", "10.244.2.2"},
				{"10.244.2.2", "10.244.1.2"},
				{"10.244.3.2", "10.244.2.2"},
			} {
				if got := forwardVerdict(t, out, pair[0], pair[1], "new"); got != "drop" {
					t.Fatalf("distinct scopes %q/%q and %q/%q: %s -> %s = %s, want drop\n%s",
						tc.deploymentA, tc.networkA, tc.deploymentB, tc.networkB, pair[0], pair[1], got, out)
				}
			}
			if got := forwardVerdict(t, out, "10.244.1.2", "10.244.3.2", "new"); got != "accept" {
				t.Fatalf("same-scope cross-host traffic = %s, want accept\n%s", got, out)
			}
		})
	}
}

func TestRender_ConntrackCannotBypassScopes(t *testing.T) {
	out := renderRules(t, firewall.RuleInput{Subnets: []firewall.Subnet{
		{Deployment: "demo", Network: "private-net", CIDR: "10.244.1.0/24"},
		{Deployment: "demo", Network: "private.net", CIDR: "10.244.2.0/24"},
		{Deployment: "demo", Network: "private-net", CIDR: "10.244.3.0/24"},
	}})
	for _, state := range []string{"new", "established", "related"} {
		t.Run(state, func(t *testing.T) {
			for _, tc := range []struct {
				src, dst, want string
			}{
				{"10.244.1.2", "10.244.2.2", "drop"},
				{"10.244.2.2", "10.244.1.2", "drop"},
				{"10.244.3.2", "10.244.2.2", "drop"},
				{"10.244.1.2", "10.244.3.2", "accept"},
				{"10.244.3.2", "10.244.1.2", "accept"},
				{"10.244.1.2", "203.0.113.2", "accept"},
				{"203.0.113.2", "10.244.1.2", "accept"},
				{"10.99.0.1", "10.244.1.2", "accept"},
				{"192.0.2.2", "203.0.113.2", "accept"},
			} {
				if got := forwardVerdict(t, out, tc.src, tc.dst, state); got != tc.want {
					t.Errorf("%s %s -> %s = %s, want %s", state, tc.src, tc.dst, got, tc.want)
				}
			}
		})
	}
}

// Evaluate only the emitted forward predicates, failing on unrecognized syntax.
// The privileged rig remains the check against actual nftables semantics.
func forwardVerdict(t *testing.T, rendered, src, dst, state string) string {
	t.Helper()
	sets := map[string][]netip.Prefix{}
	setRE := regexp.MustCompile(`(?s)    set ([a-zA-Z0-9_]+) \{.*?elements = \{ ([^}]+) \}\s+\}`)
	for _, match := range setRE.FindAllStringSubmatch(rendered, -1) {
		if _, exists := sets[match[1]]; exists {
			t.Fatalf("duplicate set %s", match[1])
		}
		for _, cidr := range strings.Split(match[2], ", ") {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				t.Fatalf("invalid rendered CIDR %q: %v", cidr, err)
			}
			sets[match[1]] = append(sets[match[1]], prefix)
		}
	}
	contains := func(name, address string) bool {
		prefixes, ok := sets[strings.TrimPrefix(name, "@")]
		if !ok {
			t.Fatalf("rule references missing set %q", name)
		}
		for _, prefix := range prefixes {
			if prefix.Contains(netip.MustParseAddr(address)) {
				return true
			}
		}
		return false
	}
	_, chain, ok := strings.Cut(rendered, "    chain forward {\n")
	if !ok {
		t.Fatal("missing forward chain")
	}
	chain, _, ok = strings.Cut(chain, "\n    }")
	if !ok {
		t.Fatal("unterminated forward chain")
	}
	for _, line := range strings.Split(chain, "\n") {
		line = strings.TrimSpace(line)
		switch line {
		case "type filter hook forward priority 0; policy accept;":
			continue
		case "ct state established,related accept":
			if state == "established" || state == "related" {
				return "accept"
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 7 || fields[0] != "ip" || fields[1] != "saddr" ||
			fields[3] != "ip" || fields[4] != "daddr" ||
			(fields[6] != "accept" && fields[6] != "drop") {
			t.Fatalf("unrecognized forward predicate %q", line)
		}
		if contains(fields[2], src) && contains(fields[5], dst) {
			return fields[6]
		}
	}
	return "accept"
}
