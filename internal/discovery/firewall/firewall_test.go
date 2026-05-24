package firewall_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
)

func TestRender_GoldenTwoDepsTwoNets(t *testing.T) {
	in := firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "sample", Network: "frontend", CIDR: "10.244.0.0/24"},
			{Deployment: "sample", Network: "backend", CIDR: "10.244.1.0/24"},
			{Deployment: "other", Network: "default", CIDR: "10.244.2.0/24"},
			{Deployment: "other", Network: "metrics", CIDR: "10.244.3.0/24"},
		},
		WGPort:       51820,
		GrpcPort:     7000,
		IngressPorts: []int{80, 443},
	}
	got := firewall.Render(in)

	goldenPath := filepath.Join("testdata", "2dep-2net.nft")
	if regenGolden() {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("Render output diverges from golden (run with REGEN_GOLDEN=1 to refresh)")
		t.Logf("=== got:\n%s\n=== want:\n%s", got, string(want))
	}
}

func regenGolden() bool { return os.Getenv("REGEN_GOLDEN") == "1" }

// TestRender_GroupsPerHostCIDRsIntoOneSet — per-host /24s (issue #28) for the
// same (deployment, network) collapse into a single nftables set with all
// elements and exactly one forward accept rule, so cross-host intra-deployment
// traffic is accepted (saddr in one host's /24, daddr in another's).
func TestRender_GroupsPerHostCIDRsIntoOneSet(t *testing.T) {
	out := firewall.Render(firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "app", Network: "frontend", CIDR: "10.244.6.0/24"},
			{Deployment: "app", Network: "frontend", CIDR: "10.244.5.0/24"},
		},
	})
	setName := firewall.SetName("app", "frontend")
	if c := strings.Count(out, "set "+setName+" {"); c != 1 {
		t.Errorf("set %s defined %d times, want 1", setName, c)
	}
	// Elements are sorted, so 5 precedes 6 regardless of input order.
	if !strings.Contains(out, "elements = { 10.244.5.0/24, 10.244.6.0/24 }") {
		t.Errorf("per-host CIDRs not grouped into one set:\n%s", out)
	}
	rule := fmt.Sprintf("ip saddr @%s ip daddr @%s accept", setName, setName)
	if c := strings.Count(out, rule); c != 1 {
		t.Errorf("forward rule for %s appears %d times, want 1", setName, c)
	}
}

// TestRender_GoldenPerHostSubnets pins the multi-element-set output.
func TestRender_GoldenPerHostSubnets(t *testing.T) {
	in := firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "app", Network: "frontend", CIDR: "10.244.5.0/24"},
			{Deployment: "app", Network: "frontend", CIDR: "10.244.6.0/24"},
			{Deployment: "app", Network: "backend", CIDR: "10.244.7.0/24"},
		},
		WGPort:       51820,
		GrpcPort:     7000,
		IngressPorts: []int{80, 443},
	}
	got := firewall.Render(in)
	goldenPath := filepath.Join("testdata", "perhost.nft")
	if regenGolden() {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("Render output diverges from golden (run with REGEN_GOLDEN=1 to refresh)")
		t.Logf("=== got:\n%s\n=== want:\n%s", got, string(want))
	}
}

func TestRender_SortsSubnetsDeterministically(t *testing.T) {
	a := firewall.Render(firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "a", Network: "y", CIDR: "10.244.0.0/24"},
			{Deployment: "b", Network: "x", CIDR: "10.244.1.0/24"},
			{Deployment: "a", Network: "x", CIDR: "10.244.2.0/24"},
		},
	})
	b := firewall.Render(firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "a", Network: "x", CIDR: "10.244.2.0/24"},
			{Deployment: "b", Network: "x", CIDR: "10.244.1.0/24"},
			{Deployment: "a", Network: "y", CIDR: "10.244.0.0/24"},
		},
	})
	if a != b {
		t.Errorf("Render not deterministic under subnet ordering")
	}
	// And that the (a,x) entry appears before (a,y), and both before (b,x).
	ax := strings.Index(a, "10.244.2.0/24")
	ay := strings.Index(a, "10.244.0.0/24")
	bx := strings.Index(a, "10.244.1.0/24")
	if !(ax < ay && ay < bx) {
		t.Errorf("subnets not sorted: ax=%d ay=%d bx=%d", ax, ay, bx)
	}
}

func TestSetName_SanitizesAndFitsLimit(t *testing.T) {
	cases := []struct {
		dep, net string
		match    string
	}{
		{"sample", "frontend", "dep_net_sample_frontend"},
		{"my-dep.v1", "front-end", "dep_net_my_dep_v1_front_end"},
		{"sample", "default", "dep_net_sample_default"},
	}
	for _, c := range cases {
		got := firewall.SetName(c.dep, c.net)
		if got != c.match {
			t.Errorf("SetName(%q,%q) = %q, want %q", c.dep, c.net, got, c.match)
		}
		if len(got) > firewall.MaxSetNameLen {
			t.Errorf("SetName(%q,%q) length %d > %d", c.dep, c.net, len(got), firewall.MaxSetNameLen)
		}
	}
}

func TestSetName_HashesWhenTooLong(t *testing.T) {
	dep := strings.Repeat("a", 40)
	net := strings.Repeat("b", 40)
	got := firewall.SetName(dep, net)
	if len(got) > firewall.MaxSetNameLen {
		t.Errorf("SetName too long: %d > %d", len(got), firewall.MaxSetNameLen)
	}
	if !strings.HasPrefix(got, "dep_net_") {
		t.Errorf("SetName lost prefix: %q", got)
	}
	// Same input deterministically hashes to the same name.
	if firewall.SetName(dep, net) != got {
		t.Errorf("SetName hashing not deterministic")
	}
}

func TestSetName_DefaultsEmptyNetworkTo_default(t *testing.T) {
	got := firewall.SetName("sample", "")
	if !strings.Contains(got, "_default") {
		t.Errorf("SetName(%q,'') = %q; expected _default fallback", "sample", got)
	}
}

func TestRender_RulesetContainsExpectedChainsAndElements(t *testing.T) {
	in := firewall.RuleInput{
		Subnets: []firewall.Subnet{{Deployment: "sample", Network: "frontend", CIDR: "10.244.0.0/24"}},
		WGPort:  51820, GrpcPort: 7000, IngressPorts: []int{80, 443},
	}
	got := firewall.Render(in)
	for _, want := range []string{
		"table inet jaco {",
		"set dep_net_sample_frontend {",
		"elements = { 10.244.0.0/24 }",
		"set jaco_pool {",
		// forward: isolate JACO's own pool, accept everything else.
		"chain forward {",
		"ct state established,related accept",
		"ip saddr @dep_net_sample_frontend ip daddr @dep_net_sample_frontend accept",
		"ip saddr @jaco_pool ip daddr @jaco_pool drop",
		// both base chains are policy accept — JACO never blanket-drops host
		// ingress or non-JACO forwarded traffic.
		"type filter hook forward priority 0; policy accept;",
		"chain input {",
		"type filter hook input priority 0; policy accept;",
		"chain output {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ruleset missing %q in:\n%s", want, got)
		}
	}
	// JACO must NOT police host ingress — no policy-drop, no management-plane
	// allowlist (the operator's SSH port / VPN / VNet / Tailscale are unknown).
	for _, forbidden := range []string{"policy drop;", "tcp dport 22", "tailscale0", "iif lo accept"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("ruleset must not contain %q (disrupts operator host choices):\n%s", forbidden, got)
		}
	}
}

func TestRender_DoesNotDropWhenNoSubnets(t *testing.T) {
	// With no JACO subnets there is nothing to isolate, so the forward chain
	// carries no pool drop — and still never a policy-drop.
	got := firewall.Render(firewall.RuleInput{})
	if strings.Contains(got, "policy drop;") {
		t.Errorf("empty input must not produce a policy-drop chain:\n%s", got)
	}
	if strings.Contains(got, "jaco_pool") {
		t.Errorf("no subnets should mean no jaco_pool set:\n%s", got)
	}
}

func TestSelfTestFromJSON_AllChainsAndSetsPresent(t *testing.T) {
	expected := firewall.RuleInput{
		Subnets: []firewall.Subnet{{Deployment: "sample", Network: "frontend", CIDR: "10.244.0.0/24"}},
	}
	jsonOK := []byte(`{"nftables":[
		{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"drop"}},
		{"chain":{"family":"inet","table":"jaco","name":"input","hook":"input","prio":0,"policy":"drop"}},
		{"chain":{"family":"inet","table":"jaco","name":"output","hook":"output","prio":0,"policy":"accept"}},
		{"set":{"family":"inet","table":"jaco","name":"dep_net_sample_frontend","type":"ipv4_addr"}}
	]}`)
	if err := firewall.SelfTestFromJSON(jsonOK, expected); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
}

func TestSelfTestFromJSON_MissingChainErrors(t *testing.T) {
	expected := firewall.RuleInput{Subnets: []firewall.Subnet{{Deployment: "a", Network: "b", CIDR: "10.244.0.0/24"}}}
	jsonMissing := []byte(`{"nftables":[
		{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"drop"}},
		{"set":{"family":"inet","table":"jaco","name":"dep_net_a_b","type":"ipv4_addr"}}
	]}`)
	err := firewall.SelfTestFromJSON(jsonMissing, expected)
	if err == nil {
		t.Fatalf("expected SelfTestError")
	}
	var ste *firewall.SelfTestError
	if !errors.As(err, &ste) {
		t.Fatalf("err is not SelfTestError: %T %v", err, err)
	}
	if ste.Code != "isolation_self_test_failed" {
		t.Errorf("code = %q", ste.Code)
	}
	if len(ste.Missing) == 0 {
		t.Errorf("Missing should list the absent chains")
	}
}

func TestSelfTestFromJSON_ExtraSetErrors(t *testing.T) {
	expected := firewall.RuleInput{Subnets: []firewall.Subnet{{Deployment: "a", Network: "b", CIDR: "10.244.0.0/24"}}}
	jsonExtra := []byte(`{"nftables":[
		{"chain":{"family":"inet","table":"jaco","name":"forward","hook":"forward","prio":0,"policy":"drop"}},
		{"chain":{"family":"inet","table":"jaco","name":"input","hook":"input","prio":0,"policy":"drop"}},
		{"chain":{"family":"inet","table":"jaco","name":"output","hook":"output","prio":0,"policy":"accept"}},
		{"set":{"family":"inet","table":"jaco","name":"dep_net_a_b","type":"ipv4_addr"}},
		{"set":{"family":"inet","table":"jaco","name":"orphan_set","type":"ipv4_addr"}}
	]}`)
	err := firewall.SelfTestFromJSON(jsonExtra, expected)
	var ste *firewall.SelfTestError
	if !errors.As(err, &ste) {
		t.Fatalf("expected SelfTestError; got %v", err)
	}
	found := false
	for _, e := range ste.Extra {
		if e == "set:orphan_set" {
			found = true
		}
	}
	if !found {
		t.Errorf("orphan_set not flagged as extra: %v", ste.Extra)
	}
}

func TestApply_FailsWhenNftMissing(t *testing.T) {
	// In CI without nftables, Apply errors with a wrapped exec failure.
	// We only verify the file-management path: a temp file gets created and
	// cleaned up. (The exec error message itself depends on the host.)
	if err := firewall.IsAvailable(); err == nil {
		t.Skip("nft is available on PATH; this test asserts the missing-binary path")
	}
	err := firewall.NftApply(context.Background(), "table inet test {}\n")
	if err == nil {
		t.Errorf("expected error when nft not on PATH")
	}
}

func TestIsAvailable_DependsOnHostPATH(t *testing.T) {
	// Just verify the call returns either nil or ErrNftNotFound (sentinel).
	err := firewall.IsAvailable()
	if err != nil && !errors.Is(err, firewall.ErrNftNotFound) {
		t.Errorf("unexpected IsAvailable error type: %v", err)
	}
	_ = fmt.Sprintf // silence
}
