//go:build nftables

package firewall_test

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
)

// TestIntegration_RenderApplySelfTest exercises the real nftables stack:
// Render → Apply (via `nft -f -`) → SelfTest (via `nft -j list`) → teardown.
// Skipped when JACO_INTEGRATION_NFTABLES is unset or nft can't be reached.
// Needs CAP_NET_ADMIN — most useful on a privileged CI runner.
func TestIntegration_RenderApplySelfTest(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_NFTABLES") == "" {
		t.Skip("set JACO_INTEGRATION_NFTABLES=1 to enable")
	}
	if err := firewall.IsAvailable(); err != nil {
		t.Skipf("firewall unavailable: %v", err)
	}

	in := firewall.RuleInput{
		Subnets: []firewall.Subnet{
			{Deployment: "integration", Network: "_default", CIDR: "10.244.42.0/24"},
		},
		WGPort:       51820,
		GrpcPort:     7000,
		IngressPorts: []int{80, 443},
	}

	t.Cleanup(func() {
		_ = exec.Command("nft", "delete", "table", "inet", "jaco").Run()
	})

	ruleset := firewall.Render(in)
	if err := firewall.NftApply(context.Background(), ruleset); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := firewall.SelfTest(context.Background(), in); err != nil {
		t.Errorf("SelfTest mismatch after Apply: %v", err)
	}
}
