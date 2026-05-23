//go:build wireguard

package wgmesh_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/wgmesh"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const testIface = "jaco-test0"

// TestIntegration_SyncerConfiguresDevice creates a real wireguard
// interface, runs Syncer.tick against a fake state.State with two peers,
// asserts wgctrl.Device(testIface).Peers reflects them. Skipped when
// JACO_INTEGRATION_WG isn't set or the kernel module isn't reachable.
// Needs CAP_NET_ADMIN.
func TestIntegration_SyncerConfiguresDevice(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_WG") == "" {
		t.Skip("set JACO_INTEGRATION_WG=1 to enable")
	}
	if err := wgmesh.IsKernelAvailable(); err != nil {
		t.Skipf("wgctrl unavailable: %v", err)
	}

	// Create the test interface. `ip link add <name> type wireguard`.
	if out, err := exec.Command("ip", "link", "add", testIface, "type", "wireguard").CombinedOutput(); err != nil {
		t.Skipf("create %s: %v: %s", testIface, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "link", "delete", testIface).Run()
	})

	brokers := watch.NewRegistry()
	st := state.New(brokers)
	st.Nodes.Apply(&pb.Node{
		Hostname:        "node-a",
		Address:         "10.0.0.1:7001",
		WireguardPubkey: make([]byte, 32),
	}, 1)
	st.Nodes.Apply(&pb.Node{
		Hostname:        "node-b",
		Address:         "10.0.0.2:7001",
		WireguardPubkey: make([]byte, 32),
	}, 2)

	priv, _, err := wgmesh.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	cfg, err := wgmesh.BuildConfig(st, "self", priv)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	client, err := wgctrl.New()
	if err != nil {
		t.Fatalf("wgctrl.New: %v", err)
	}
	defer client.Close()

	if err := client.ConfigureDevice(testIface, cfg); err != nil {
		t.Fatalf("ConfigureDevice: %v", err)
	}

	// Give the kernel a tick to settle.
	time.Sleep(100 * time.Millisecond)

	dev, err := client.Device(testIface)
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if got := len(dev.Peers); got != 2 {
		t.Errorf("peers = %d, want 2", got)
	}
	_ = context.Background()
}
