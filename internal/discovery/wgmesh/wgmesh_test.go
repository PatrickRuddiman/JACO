package wgmesh_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/wgmesh"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newState() *state.State {
	return state.New(watch.NewRegistry())
}

func TestGenerateKeypair_ProducesValidKeys(t *testing.T) {
	priv, pub, err := wgmesh.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if len(priv) != wgtypes.KeyLen || len(pub) != wgtypes.KeyLen {
		t.Errorf("key length wrong: priv=%d pub=%d", len(priv), len(pub))
	}
	if priv == pub {
		t.Errorf("priv == pub; expected derived public key to differ")
	}
}

func TestLoadOrGenerateKeypair_PersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	priv1, pub1, err := wgmesh.LoadOrGenerateKeypair(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	priv2, pub2, err := wgmesh.LoadOrGenerateKeypair(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if priv1 != priv2 || pub1 != pub2 {
		t.Errorf("keys not stable across calls")
	}
	// File mode is 0600.
	info, err := os.Stat(filepath.Join(dir, "wg", "private.key"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private.key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSlotIP_ProducesAddressInsideMeshNetwork(t *testing.T) {
	mesh, _ := parseCIDR(wgmesh.MeshNetwork)
	for _, hostname := range []string{"node-a", "node-b", "node-c", "controller-1", "worker-7"} {
		ip := wgmesh.SlotIP(hostname)
		if !mesh.Contains(ip) {
			t.Errorf("SlotIP(%q) = %s; not inside %s", hostname, ip, wgmesh.MeshNetwork)
		}
		// Confirm the third octet is fixed.
		ip4 := ip.To4()
		if ip4 == nil || ip4[0] != 10 || ip4[1] != 99 || ip4[2] != 0 {
			t.Errorf("SlotIP(%q) = %s; expected 10.99.0.<n>", hostname, ip)
		}
		// Last octet is in [1, 254].
		if ip4[3] == 0 || ip4[3] == 255 {
			t.Errorf("SlotIP(%q) = %s; fourth octet must avoid network (0) / broadcast (255)", hostname, ip)
		}
	}
}

func TestSlotIP_IsDeterministic(t *testing.T) {
	a := wgmesh.SlotIP("node-a")
	for i := 0; i < 100; i++ {
		if !wgmesh.SlotIP("node-a").Equal(a) {
			t.Fatalf("SlotIP not deterministic")
		}
	}
}

func TestAllowedIP_HasSlash32(t *testing.T) {
	got := wgmesh.AllowedIP("node-a")
	if !strings.HasSuffix(got, "/32") {
		t.Errorf("AllowedIP = %q; want /32 suffix", got)
	}
}

func TestRenderConfig_EmitsInterfaceAndOneSection(t *testing.T) {
	st := newState()
	st.Nodes.Apply(&pb.Node{
		Hostname: "node-a", Address: "10.0.0.1:7000", Status: pb.NodeStatus_NODE_STATUS_READY,
	}, 1)
	// One peer with a real pubkey.
	priv, _ := wgtypes.GeneratePrivateKey()
	st.Nodes.Apply(&pb.Node{
		Hostname: "node-b", Address: "10.0.0.2:7000",
		WireguardPubkey: keyBytes(priv.PublicKey()),
		Status:          pb.NodeStatus_NODE_STATUS_READY,
	}, 2)

	selfPriv, _ := wgtypes.GeneratePrivateKey()
	cfg, err := wgmesh.RenderConfig(st, "node-a", selfPriv)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	for _, want := range []string{
		"[Interface]",
		"PrivateKey = " + selfPriv.String(),
		"ListenPort = 51820",
		"[Peer]",
		"# node-b",
		"PublicKey = " + priv.PublicKey().String(),
		"Endpoint = 10.0.0.2:51820",
		"AllowedIPs =",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
}

func TestRenderConfig_OmitsSelfAndUnregisteredPeers(t *testing.T) {
	st := newState()
	priv, _ := wgtypes.GeneratePrivateKey()
	// Three nodes: self + one registered peer + one without a pubkey yet.
	st.Nodes.Apply(&pb.Node{Hostname: "node-a", Address: "10.0.0.1:7000"}, 1)
	st.Nodes.Apply(&pb.Node{Hostname: "node-b", Address: "10.0.0.2:7000", WireguardPubkey: keyBytes(priv.PublicKey())}, 2)
	st.Nodes.Apply(&pb.Node{Hostname: "node-c", Address: "10.0.0.3:7000"}, 3) // no pubkey

	cfg, err := wgmesh.RenderConfig(st, "node-a", priv)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if strings.Contains(cfg, "# node-a") {
		t.Errorf("self appears as [Peer] in config")
	}
	if strings.Contains(cfg, "# node-c") {
		t.Errorf("unregistered peer (no pubkey) appears in config")
	}
	if strings.Count(cfg, "[Peer]") != 1 {
		t.Errorf("expected 1 [Peer] section; got %d", strings.Count(cfg, "[Peer]"))
	}
}

func TestRenderConfig_ProducesDeterministicPeerOrdering(t *testing.T) {
	st := newState()
	a, _ := wgtypes.GeneratePrivateKey()
	b, _ := wgtypes.GeneratePrivateKey()
	c, _ := wgtypes.GeneratePrivateKey()
	st.Nodes.Apply(&pb.Node{Hostname: "node-c", Address: "10.0.0.3:7000", WireguardPubkey: keyBytes(c.PublicKey())}, 1)
	st.Nodes.Apply(&pb.Node{Hostname: "node-a", Address: "10.0.0.1:7000", WireguardPubkey: keyBytes(a.PublicKey())}, 2)
	st.Nodes.Apply(&pb.Node{Hostname: "node-b", Address: "10.0.0.2:7000", WireguardPubkey: keyBytes(b.PublicKey())}, 3)
	selfPriv, _ := wgtypes.GeneratePrivateKey()

	cfg, _ := wgmesh.RenderConfig(st, "self-node", selfPriv)
	idxA := strings.Index(cfg, "# node-a")
	idxB := strings.Index(cfg, "# node-b")
	idxC := strings.Index(cfg, "# node-c")
	if !(idxA < idxB && idxB < idxC) {
		t.Errorf("peers not alphabetically ordered: a=%d b=%d c=%d", idxA, idxB, idxC)
	}
}

func TestRenderConfig_RejectsBadPubkeyLength(t *testing.T) {
	st := newState()
	st.Nodes.Apply(&pb.Node{Hostname: "node-b", Address: "10.0.0.2:7000", WireguardPubkey: []byte{0x01, 0x02}}, 1)
	selfPriv, _ := wgtypes.GeneratePrivateKey()
	if _, err := wgmesh.RenderConfig(st, "self", selfPriv); err == nil {
		t.Errorf("expected error on short pubkey")
	}
}

func TestRenderConfig_RejectsEmptySelfHostname(t *testing.T) {
	st := newState()
	selfPriv, _ := wgtypes.GeneratePrivateKey()
	if _, err := wgmesh.RenderConfig(st, "", selfPriv); err == nil {
		t.Errorf("expected error on empty hostname")
	}
}

// --- helpers -----------------------------------------------------------------

func parseCIDR(s string) (*net.IPNet, error) {
	_, n, err := net.ParseCIDR(s)
	return n, err
}

// keyBytes copies a wgtypes.Key into a []byte (the value type isn't directly
// sliceable).
func keyBytes(k wgtypes.Key) []byte {
	out := make([]byte, len(k))
	copy(out, k[:])
	return out
}
