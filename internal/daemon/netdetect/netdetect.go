// Package netdetect picks a single routable IPv4 address to advertise to
// cluster peers when jacod's bind address is unspecified (0.0.0.0 / ::).
//
// raft refuses to start when its bind address is unspecified — peers
// would try to dial 0.0.0.0. The *advertise* face has to be a specific
// reachable IP, and jacod also binds its control/data plane to that face
// (gRPC + raft) so the cluster planes are not world-reachable on a public
// NIC by default. This package finds the face to use.
//
// Priority (lowest class wins; alphabetical interface name breaks ties):
//
//  1. Private LAN — RFC1918: 10/8, 172.16/12, 192.168/16
//  2. Overlay     — tailscale ("tailscale*" / 100.64/10) or tun/tap/wg/jaco
//
// A host that has a private VNet *and* an overlay (e.g. Tailscale) now
// advertises the private VNet face — the operator's own LAN is treated
// as the cluster fabric unless they pin an explicit listen_addr /
// cluster_addr to override.
//
// Public IPv4 is NEVER auto-picked: the mesh control/data plane must not
// be advertised on (or bound to) a world-reachable address by default.
// A host whose only routable address is public yields ErrNoCandidate, so
// the operator is forced to pin an explicit address rather than silently
// exposing the cluster planes. (A cluster whose nodes share no LAN must
// be wired over an overlay or an explicit pin — see the daemon config.)
//
// Skipped: public IPv4, loopback (127/8), link-local (169.254/16),
// multicast, interfaces marked down. IPv6 is out of scope for v0.
package netdetect

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// ErrNoCandidate is returned when no usable IP could be found across all
// up, non-loopback interfaces.
var ErrNoCandidate = errors.New("no routable IPv4 interface address found")

// PickAdvertiseIP scans the host's interfaces and returns the highest-
// priority IPv4 address per the package docs. Production entry point.
func PickAdvertiseIP() (net.IP, string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, "", fmt.Errorf("list interfaces: %w", err)
	}
	return pickFromInterfaces(ifaces, func(i net.Interface) ([]net.Addr, error) {
		return i.Addrs()
	})
}

// pickFromInterfaces is the testable core: takes an explicit interface
// list + an addrs-fetcher so unit tests can inject synthetic data.
//
// Returns (ip, ifname, err). ifname is the interface the winning IP
// came from, useful in logs.
func pickFromInterfaces(ifaces []net.Interface, addrsOf func(net.Interface) ([]net.Addr, error)) (net.IP, string, error) {
	type candidate struct {
		class int
		name  string
		ip    net.IP
	}
	var cands []candidate

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := addrsOf(iface)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			c := classify(iface.Name, ip)
			if c == 0 {
				continue
			}
			cands = append(cands, candidate{class: c, name: iface.Name, ip: ip})
		}
	}

	if len(cands) == 0 {
		return nil, "", ErrNoCandidate
	}

	// Lowest class wins (1=private LAN, 2=overlay); alphabetical interface
	// name as deterministic tiebreak; IP string as final tiebreak when one
	// iface has multiple IPs in the same class.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].class != cands[j].class {
			return cands[i].class < cands[j].class
		}
		if cands[i].name != cands[j].name {
			return cands[i].name < cands[j].name
		}
		return cands[i].ip.String() < cands[j].ip.String()
	})

	w := cands[0]
	return w.ip, w.name, nil
}

// classify returns the priority class (1=private LAN, 2=overlay) for a
// usable address, or 0 for anything to skip (public / loopback /
// link-local / multicast / etc.).
//
// Lower class wins. Private LAN beats overlay so a host with both a VNet
// and a tailnet advertises its operator-owned LAN by default. Public
// IPv4 is deliberately class 0 (skipped) — never auto-pick a
// world-reachable face for the mesh; the operator must pin one instead.
//
// ip MUST already be net.IP.To4() — IPv6 returns 0.
func classify(ifaceName string, ip net.IP) int {
	if ip == nil || ip.To4() == nil {
		return 0
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return 0
	}

	// Class 2 — overlay devices, identified by interface name. These are
	// explicit overlay/VPN/mesh faces (tailscale, tun, tap, wg, jaco), so
	// they're classified by role regardless of which range their address
	// falls in. Checked before the RFC1918 test so an overlay carrying a
	// private-range address (e.g. wg0 on 10.244/16) is still treated as the
	// overlay fabric, not as a peer's operator VNet. A non-overlay iface
	// holding a CGNAT (100.64/10) address — tailscale userspace without the
	// canonical name — also lands here.
	if isOverlayName(ifaceName) || inCIDR(ip, "100.64.0.0/10") {
		return 2
	}

	// Class 1 — private LAN (RFC1918) on a non-overlay interface. The
	// operator's own VNet/LAN is the preferred cluster fabric and beats an
	// overlay when both are present.
	if inCIDR(ip, "10.0.0.0/8") || inCIDR(ip, "172.16.0.0/12") || inCIDR(ip, "192.168.0.0/16") {
		return 1
	}

	// Public / anything else routable — never auto-pick for the mesh.
	return 0
}

// isOverlayName reports whether an interface name denotes an overlay /
// VPN / mesh device (tailscale, tun, tap, wg, jaco).
func isOverlayName(ifaceName string) bool {
	if strings.HasPrefix(ifaceName, "tailscale") {
		return true
	}
	for _, p := range []string{"tun", "tap", "wg", "jaco"} {
		if strings.HasPrefix(ifaceName, p) {
			return true
		}
	}
	return false
}

// inCIDR is a small helper that hides the parse-fail-can't-happen
// branch from callers.
func inCIDR(ip net.IP, cidr string) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return n.Contains(ip)
}
