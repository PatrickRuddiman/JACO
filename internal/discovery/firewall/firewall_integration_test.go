//go:build nftables

package firewall_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
)

func TestIntegration_RenderApplySelfTest(t *testing.T) {
	if !nftTestNamespace(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	in := firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "demo", Network: "private-net", CIDR: "10.244.1.0/24"},
			{Deployment: "demo", Network: "private.net", CIDR: "10.244.2.0/24"},
			{Deployment: "demo", Network: "private-net", CIDR: "10.244.3.0/24"},
			{Deployment: "a-b", Network: "c", CIDR: "10.244.4.0/24"},
			{Deployment: "a", Network: "b-c", CIDR: "10.244.5.0/24"},
		},
	}

	ruleset, err := firewall.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Cold creation, migration from a merged legacy set, and repeated reload.
	if err := firewall.NftApply(ctx, ruleset); err != nil {
		t.Fatalf("cold Apply: %v", err)
	}
	if err := firewall.SelfTest(ctx, in); err != nil {
		t.Fatalf("cold SelfTest: %v", err)
	}
	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy-colliding.nft"))
	if err != nil {
		t.Fatal(err)
	}
	if err := firewall.NftApply(ctx, string(legacy)); err != nil {
		t.Fatalf("legacy Apply: %v", err)
	}
	before, err := firewall.NftList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := firewall.SelfTestFromJSON(before, in); err == nil {
		t.Fatal("legacy merged set must require migration")
	}
	invalid := ruleset + "\nadd rule inet jaco forward ip saddr @missing_scope_set accept\n"
	if err := firewall.NftApply(ctx, invalid); err == nil {
		t.Fatal("invalid transaction unexpectedly succeeded")
	}
	after, err := firewall.NftList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected atomic replacement changed the live table")
	}
	for i := 0; i < 2; i++ {
		if err := firewall.NftApply(ctx, ruleset); err != nil {
			t.Fatalf("migration/reload Apply: %v", err)
		}
		if err := firewall.SelfTest(ctx, in); err != nil {
			t.Fatalf("SelfTest mismatch after migration/reload: %v", err)
		}
		live, err := firewall.NftList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertNftScopeSets(t, live, in)
	}
}

// Re-exec in a fresh network namespace before any nft mutation. A failed
// unshare is an error, never a fallback to the caller's live firewall.
func nftTestNamespace(t *testing.T) bool {
	t.Helper()
	if os.Getenv("JACO_INTEGRATION_NFTABLES") == "" {
		t.Skip("set JACO_INTEGRATION_NFTABLES=1 to enable")
	}
	if runtime.GOOS != "linux" {
		t.Skip("nftables integration requires Linux network namespaces")
	}
	if err := firewall.IsAvailable(); err != nil {
		t.Skipf("firewall unavailable: %v", err)
	}
	const childEnv = "JACO_NFT_TEST_NETNS"
	if os.Getenv(childEnv) == t.Name() {
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "--net", "--", executable,
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated nftables test: %v\n%s", err, out)
	}
	t.Logf("%s", out)
	return false
}

func assertNftScopeSets(t *testing.T, data []byte, in firewall.RuleInput) {
	t.Helper()
	var doc struct {
		Nftables []struct {
			Set *struct {
				Name string          `json:"name"`
				Elem json.RawMessage `json:"elem"`
			} `json:"set"`
			Rule *struct {
				Chain string `json:"chain"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"jaco_pool": nil}
	for _, subnet := range in.Subnets {
		name := firewall.SetName(subnet.Deployment, subnet.Network)
		want[name] = append(want[name], subnet.CIDR)
	}
	got := map[string][]string{}
	forwardRules := 0
	for _, entry := range doc.Nftables {
		if entry.Set != nil {
			got[entry.Set.Name] = nil
			if entry.Set.Name != "jaco_pool" {
				var elements []struct {
					Prefix struct {
						Addr string `json:"addr"`
						Len  int    `json:"len"`
					} `json:"prefix"`
				}
				if err := json.Unmarshal(entry.Set.Elem, &elements); err != nil {
					t.Fatal(err)
				}
				for _, element := range elements {
					got[entry.Set.Name] = append(got[entry.Set.Name],
						fmt.Sprintf("%s/%d", element.Prefix.Addr, element.Prefix.Len))
				}
			}
		}
		if entry.Rule != nil && entry.Rule.Chain == "forward" {
			forwardRules++
		}
	}
	for _, cidrs := range want {
		slices.Sort(cidrs)
	}
	for _, cidrs := range got {
		slices.Sort(cidrs)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("live nft sets = %v, want %v", got, want)
	}
	if forwardRules != len(want) {
		t.Errorf("forward rules = %d, want one per scope plus one pool drop (%d)", forwardRules, len(want))
	}
}
