// Package netdetect picks a single routable IPv4 address to advertise to
// cluster peers when jacod's bind address is unspecified (0.0.0.0 / ::).
//
// raft refuses to start when its bind address is unspecified — peers
// would try to dial 0.0.0.0. The daemon binds on all interfaces by
// default (correct for accepting connections), but the *advertise*
// face has to be a specific reachable IP. This package finds one.
//
// Priority (lowest class wins; alphabetical interface name breaks ties):
//
//	1. Tailscale  — name starts with "tailscale", OR IPv4 in 100.64.0.0/10
//	2. Tunnel/VPN — name starts with "tun", "tap", "wg", "jaco"
//	3. RFC1918    — 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16
//	4. Public     — any other non-loopback, non-link-local IPv4
//
// Skipped: loopback (127/8), link-local (169.254/16), multicast,
// interfaces marked down. IPv6 is out of scope for v0.
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

	// Lowest class wins; alphabetical interface name as deterministic
	// tiebreak; IP string as final tiebreak when one iface has multiple
	// IPs in the same class.
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

// classify returns 1-4 for a usable address per the priority table, or 0
// for anything to skip (loopback / link-local / multicast / etc.).
//
// ip MUST already be net.IP.To4() — IPv6 returns 0.
func classify(ifaceName string, ip net.IP) int {
	if ip == nil || ip.To4() == nil {
		return 0
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return 0
	}

	// Class 1 — Tailscale by name or CGNAT range.
	if strings.HasPrefix(ifaceName, "tailscale") {
		return 1
	}
	if inCIDR(ip, "100.64.0.0/10") {
		return 1
	}

	// Class 2 — tunnel / VPN / mesh.
	for _, p := range []string{"tun", "tap", "wg", "jaco"} {
		if strings.HasPrefix(ifaceName, p) {
			return 2
		}
	}

	// Class 3 — RFC1918.
	if inCIDR(ip, "10.0.0.0/8") || inCIDR(ip, "172.16.0.0/12") || inCIDR(ip, "192.168.0.0/16") {
		return 3
	}

	// Class 4 — anything else routable.
	return 4
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
