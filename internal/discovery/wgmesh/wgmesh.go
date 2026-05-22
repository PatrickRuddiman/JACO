// Package wgmesh wires the per-node WireGuard mesh that carries inter-node
// traffic between JACO bridges. The local node owns one wg interface
// (typically `jaco0`) bound to a /32 inside `10.99.0.0/24`; peers are the
// other Node entities, with their endpoint pulled from Node.address and
// their PublicKey from Node.wireguard_pubkey.
//
// v1 covers the pure-Go primitives: keypair management, deterministic /32
// slot derivation, and rendering a wg-quick-style config from state.Nodes.
// The kernel-side Sync (calling wgctrl to install the configuration onto a
// real wg device) lands when the daemon entry comes together.
package wgmesh

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// MeshNetwork is the JACO WireGuard overlay subnet (per the discovery slice
// §3). 256 nodes fit; the third octet is fixed, the fourth is the per-node
// slot derived from the hostname.
const MeshNetwork = "10.99.0.0/24"

// DefaultListenPort is the UDP port the wg interface listens on. Matches the
// WireGuard convention (51820) and shows up in every peer Endpoint.
const DefaultListenPort = 51820

// GenerateKeypair creates a fresh Curve25519 keypair. Uses crypto/rand under
// the hood (wgtypes.GeneratePrivateKey) so it works without any kernel
// modules — ideal for the bootstrap path.
func GenerateKeypair() (private wgtypes.Key, public wgtypes.Key, err error) {
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, wgtypes.Key{}, fmt.Errorf("generate wg key: %w", err)
	}
	return priv, priv.PublicKey(), nil
}

// LoadOrGenerateKeypair reads <dataDir>/wg/private.key (base64-encoded
// wgtypes.Key). When absent, generates a fresh keypair and persists it at
// mode 0600. Returns (private, public, error).
func LoadOrGenerateKeypair(dataDir string) (wgtypes.Key, wgtypes.Key, error) {
	dir := filepath.Join(dataDir, "wg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wgtypes.Key{}, wgtypes.Key{}, fmt.Errorf("mkdir wg dir: %w", err)
	}
	path := filepath.Join(dir, "private.key")

	if data, err := os.ReadFile(path); err == nil {
		priv, perr := wgtypes.ParseKey(strings.TrimSpace(string(data)))
		if perr != nil {
			return wgtypes.Key{}, wgtypes.Key{}, fmt.Errorf("parse %s: %w", path, perr)
		}
		return priv, priv.PublicKey(), nil
	} else if !os.IsNotExist(err) {
		return wgtypes.Key{}, wgtypes.Key{}, fmt.Errorf("read %s: %w", path, err)
	}

	priv, pub, err := GenerateKeypair()
	if err != nil {
		return wgtypes.Key{}, wgtypes.Key{}, err
	}
	if err := os.WriteFile(path, []byte(priv.String()+"\n"), 0o600); err != nil {
		return wgtypes.Key{}, wgtypes.Key{}, fmt.Errorf("write %s: %w", path, err)
	}
	return priv, pub, nil
}

// SlotIP derives the per-node /32 address from the hostname. Uses the first
// byte of sha256(hostname) as the fourth octet (deterministic + uniform
// distribution; bounded collisions documented in tests). Returns the
// address WITHOUT a /prefix — caller pairs with /32 when rendering.
func SlotIP(hostname string) net.IP {
	h := sha256.Sum256([]byte(hostname))
	// Skip 0 (network) and 255 (broadcast) — wrap into [1, 254].
	o := h[0]
	if o == 0 || o == 255 {
		o = uint8(binary.BigEndian.Uint16(h[0:2])%254) + 1
	}
	return net.IPv4(10, 99, 0, o)
}

// AllowedIP returns SlotIP(hostname) formatted as a /32 string suitable for
// the `AllowedIPs = ...` line of a wg-quick config.
func AllowedIP(hostname string) string {
	return SlotIP(hostname).String() + "/32"
}

// RenderConfig renders a wg-quick-style configuration for selfHostname. The
// [Interface] block uses the local private key + the SlotIP for the address.
// One [Peer] block per other Node in state — sorted by hostname for
// deterministic output — populated with PublicKey, Endpoint, AllowedIPs,
// and PersistentKeepalive.
func RenderConfig(st *state.State, selfHostname string, selfPrivate wgtypes.Key) (string, error) {
	if selfHostname == "" {
		return "", fmt.Errorf("RenderConfig: selfHostname is required")
	}
	var b strings.Builder
	fmt.Fprintln(&b, "[Interface]")
	fmt.Fprintf(&b, "PrivateKey = %s\n", selfPrivate.String())
	fmt.Fprintf(&b, "Address = %s\n", AllowedIP(selfHostname))
	fmt.Fprintf(&b, "ListenPort = %d\n", DefaultListenPort)

	peers := make([]*pb.Node, 0)
	for _, n := range st.Nodes.List() {
		if n.GetHostname() == selfHostname {
			continue
		}
		if len(n.GetWireguardPubkey()) == 0 {
			continue
		}
		peers = append(peers, n)
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].GetHostname() < peers[j].GetHostname()
	})

	for _, n := range peers {
		pub, err := keyFromBytes(n.GetWireguardPubkey())
		if err != nil {
			return "", fmt.Errorf("peer %s pubkey: %w", n.GetHostname(), err)
		}
		host := stripPort(n.GetAddress())
		endpoint := fmt.Sprintf("%s:%d", host, DefaultListenPort)

		fmt.Fprintln(&b, "")
		fmt.Fprintf(&b, "[Peer]\n")
		fmt.Fprintf(&b, "# %s\n", n.GetHostname())
		fmt.Fprintf(&b, "PublicKey = %s\n", pub.String())
		fmt.Fprintf(&b, "Endpoint = %s\n", endpoint)
		fmt.Fprintf(&b, "AllowedIPs = %s\n", AllowedIP(n.GetHostname()))
		fmt.Fprintf(&b, "PersistentKeepalive = 25\n")
	}
	return b.String(), nil
}

// keyFromBytes adapts raw 32-byte wgtypes.Key payloads into the typed key.
func keyFromBytes(b []byte) (wgtypes.Key, error) {
	if len(b) != wgtypes.KeyLen {
		return wgtypes.Key{}, fmt.Errorf("wg pubkey must be %d bytes (got %d)", wgtypes.KeyLen, len(b))
	}
	var k wgtypes.Key
	copy(k[:], b)
	return k, nil
}

// stripPort returns the host portion of `host:port`, or the whole string if
// no port is present. Used to translate Node.address (raft TCP) into the
// host part of the WG endpoint.
func stripPort(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
