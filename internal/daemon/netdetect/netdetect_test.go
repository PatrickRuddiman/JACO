package netdetect

import (
	"errors"
	"net"
	"testing"
)

// mkIface is a small constructor for synthetic interfaces.
func mkIface(name string, flags net.Flags) net.Interface {
	return net.Interface{Name: name, Flags: flags}
}

// mkAddrs wraps IP strings into the *net.IPNet form that net.Interface.Addrs
// returns. Mask is /24 by default — the picker doesn't care about the mask.
func mkAddrs(ips ...string) []net.Addr {
	out := make([]net.Addr, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(s)
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)})
	}
	return out
}

func TestPick_LANBeatsTailscaleByName(t *testing.T) {
	// Private-LAN-first: a host with both an operator VNet (eth0) and a
	// tailnet must advertise the VNet, not the tailscale face.
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp),
		mkIface("tailscale0", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0":       mkAddrs("192.168.1.10"),
		"tailscale0": mkAddrs("100.96.1.2"),
	}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ip.String() != "192.168.1.10" || name != "eth0" {
		t.Errorf("got %s on %s; want 192.168.1.10 on eth0 (private LAN beats tailscale)", ip, name)
	}
}

func TestPick_LANBeatsTailscaleByCGNATRange(t *testing.T) {
	// Interface NOT named tailscale*, but IP is in 100.64/10 — that's an
	// overlay (class 2). The RFC1918 LAN still wins.
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp),
		mkIface("eth1", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("192.168.1.10"),
		"eth1": mkAddrs("100.127.5.5"),
	}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ip.String() != "192.168.1.10" || name != "eth0" {
		t.Errorf("got %s on %s; want 192.168.1.10 on eth0 (private LAN beats CGNAT overlay)", ip, name)
	}
}

func TestPick_OverlayBeatsPublic(t *testing.T) {
	// No private LAN present: the overlay (wg0) is the only safe fabric.
	// The public IP must never be auto-picked.
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp),
		mkIface("wg0", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("203.0.113.7"), // public — never auto-picked
		"wg0":  mkAddrs("100.64.5.5"),  // overlay (CGNAT)
	}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if name != "wg0" || ip.String() != "100.64.5.5" {
		t.Errorf("got %s on %s; want 100.64.5.5 on wg0 (overlay beats public)", ip, name)
	}
}

func TestPick_PublicAndPrivate_PrivateWinsPublicNeverPicked(t *testing.T) {
	// The headline #44 case: a host with both a public NIC and a private
	// VNet must advertise the private face, and the public IP must never be
	// selected (control/data plane stays off the world-reachable face).
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp), // public
		mkIface("eth1", net.FlagUp), // private VNet
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("198.51.100.7"),
		"eth1": mkAddrs("10.10.0.4"),
	}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ip.String() != "10.10.0.4" || name != "eth1" {
		t.Errorf("got %s on %s; want 10.10.0.4 on eth1 (private face wins)", ip, name)
	}
	if ip.String() == "198.51.100.7" {
		t.Errorf("public IP was auto-picked; must never happen")
	}
}

func TestPick_PublicOnly_ReturnsNoCandidate(t *testing.T) {
	// A host whose only routable address is public yields ErrNoCandidate:
	// jacod must NOT silently advertise/bind the mesh on a public face.
	// The operator is forced to pin an explicit listen_addr/cluster_addr.
	ifaces := []net.Interface{mkIface("eth0", net.FlagUp)}
	addrs := map[string][]net.Addr{"eth0": mkAddrs("198.51.100.7")}
	_, _, err := pickFromInterfaces(ifaces, lookup(addrs))
	if !errors.Is(err, ErrNoCandidate) {
		t.Errorf("err = %v; want ErrNoCandidate (public must never be auto-picked)", err)
	}
}

func TestPick_NoCandidates_ReturnsError(t *testing.T) {
	// Only loopback + a down interface.
	ifaces := []net.Interface{
		mkIface("lo", net.FlagUp|net.FlagLoopback),
		mkIface("eth0", 0), // down — no FlagUp
	}
	addrs := map[string][]net.Addr{
		"lo":   mkAddrs("127.0.0.1"),
		"eth0": mkAddrs("192.168.1.10"),
	}
	_, _, err := pickFromInterfaces(ifaces, lookup(addrs))
	if !errors.Is(err, ErrNoCandidate) {
		t.Errorf("err = %v; want ErrNoCandidate", err)
	}
}

func TestPick_DeterministicTiebreak(t *testing.T) {
	// Both interfaces are class 3 with the same priority — alphabetical
	// interface name decides.
	ifaces := []net.Interface{
		mkIface("eth1", net.FlagUp),
		mkIface("eth0", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("10.0.0.5"),
		"eth1": mkAddrs("10.0.0.6"),
	}
	_, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if name != "eth0" {
		t.Errorf("got iface %s; want eth0 (alphabetically first)", name)
	}
}

func TestPick_SkipsLinkLocalAndIPv6(t *testing.T) {
	ifaces := []net.Interface{mkIface("eth0", net.FlagUp)}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("169.254.1.1", "fe80::1", "10.0.0.5"),
	}
	ip, _, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ip.String() != "10.0.0.5" {
		t.Errorf("got %s; want 10.0.0.5 (link-local + v6 must be skipped)", ip)
	}
}

func TestClassify_ExhaustiveTable(t *testing.T) {
	cases := []struct {
		iface string
		ip    string
		want  int
	}{
		// Class 2 — overlay (by name or CGNAT range).
		{"tailscale0", "100.96.1.2", 2},
		{"eth0", "100.127.5.5", 2}, // CGNAT regardless of iface name
		{"wg0", "10.244.0.1", 2},   // overlay name wins over RFC1918 range
		{"tun0", "10.0.0.1", 2},
		{"tap0", "192.168.1.1", 2},
		{"jaco0", "10.0.0.99", 2},
		// Class 1 — private LAN on a non-overlay interface.
		{"eth0", "10.0.0.5", 1},
		{"eth0", "172.20.0.5", 1},
		{"eth0", "192.168.1.5", 1},
		// Class 0 — public is never auto-picked.
		{"eth0", "8.8.8.8", 0},
		{"eth0", "203.0.113.9", 0},
		// Class 0 — container / virtual bridges, even on RFC1918 ranges.
		// docker0 is 172.17.0.1 on every host; without this it would beat
		// eth0 on the alphabetical tiebreak and bind to an unreachable face.
		{"docker0", "172.17.0.1", 0},
		{"docker_gwbridge", "172.18.0.1", 0},
		{"br-3f9a2b1c4d5e", "10.244.0.1", 0}, // docker user-defined bridge
		{"veth8a1b2c3", "10.244.0.2", 0},
		{"virbr0", "192.168.122.1", 0},
		{"br0", "192.168.1.5", 1}, // operator's own bridged NIC is NOT excluded
		// Class 0 — non-routable / special.
		{"eth0", "127.0.0.1", 0},   // loopback
		{"eth0", "169.254.1.1", 0}, // link-local
		{"eth0", "224.0.0.1", 0},   // multicast
		{"eth0", "0.0.0.0", 0},     // unspecified
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip).To4()
		got := classify(c.iface, ip)
		if got != c.want {
			t.Errorf("classify(%s, %s) = %d; want %d", c.iface, c.ip, got, c.want)
		}
	}
}

func TestLocalIPs_ReturnsAllUsableIPsExcludingLoopbackAndLinkLocal(t *testing.T) {
	ifaces := []net.Interface{
		mkIface("lo", net.FlagUp|net.FlagLoopback),
		mkIface("eth0", net.FlagUp),
		mkIface("tailscale0", net.FlagUp),
		mkIface("eth1", net.FlagUp),
		mkIface("docker0", net.FlagUp), // container bridge — excluded
		mkIface("down0", 0),            // down — excluded
	}
	addrs := map[string][]net.Addr{
		"lo":         mkAddrs("127.0.0.1"),
		"eth0":       mkAddrs("192.168.1.10", "169.254.5.5", "fe80::1"), // RFC1918 + link-local + IPv6
		"tailscale0": mkAddrs("100.96.1.2"),                             // tailscale CGNAT
		"eth1":       mkAddrs("8.8.8.8"),                                // public — excluded
		"docker0":    mkAddrs("172.17.0.1"),                             // docker bridge — excluded
		"down0":      mkAddrs("10.9.9.9"),                               // on a down iface
	}

	got := localIPsFromInterfaces(ifaces, lookup(addrs))

	gotSet := make(map[string]bool, len(got))
	for _, ip := range got {
		gotSet[ip.String()] = true
	}

	// SANs follow classify()'s boundary: private LAN + overlay only. Public
	// (8.8.8.8) and the docker bridge (172.17.0.1) are excluded — a pinned
	// public advertise addr is still SAN'd via the explicit listen/cluster IP
	// at the call site, not via auto-detect.
	want := []string{"192.168.1.10", "100.96.1.2"}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("LocalIPs missing %s; got %v", w, got)
		}
	}
	excluded := []string{"8.8.8.8", "172.17.0.1", "127.0.0.1", "169.254.5.5", "fe80::1", "10.9.9.9"}
	for _, e := range excluded {
		if gotSet[e] {
			t.Errorf("LocalIPs should exclude %s; got %v", e, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("LocalIPs returned %d addrs (%v); want %d", len(got), got, len(want))
	}
}

func TestLocalIPs_DedupesAndSorts(t *testing.T) {
	// Same IP reachable via two interfaces (e.g. a bridge + its member) must
	// appear once. Output must be sorted by IP string for determinism.
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp),
		mkIface("br0", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("10.0.0.5", "192.168.1.10"),
		"br0":  mkAddrs("10.0.0.5"), // duplicate of eth0's first addr
	}

	got := localIPsFromInterfaces(ifaces, lookup(addrs))

	want := []string{"10.0.0.5", "192.168.1.10"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i, w := range want {
		if got[i].String() != w {
			t.Errorf("got[%d] = %s; want %s (sorted, deduped)", i, got[i], w)
		}
	}
}

func TestLocalIPs_NoUsableIPs_ReturnsNil(t *testing.T) {
	ifaces := []net.Interface{
		mkIface("lo", net.FlagUp|net.FlagLoopback),
		mkIface("eth0", 0), // down
	}
	addrs := map[string][]net.Addr{
		"lo":   mkAddrs("127.0.0.1"),
		"eth0": mkAddrs("192.168.1.10"),
	}
	if got := localIPsFromInterfaces(ifaces, lookup(addrs)); got != nil {
		t.Errorf("got %v; want nil", got)
	}
}

// lookup returns an addrsOf function backed by a static map. Map miss
// returns an empty slice, mirroring how net.Interface.Addrs behaves on
// hostless interfaces.
func lookup(m map[string][]net.Addr) func(net.Interface) ([]net.Addr, error) {
	return func(i net.Interface) ([]net.Addr, error) {
		if v, ok := m[i.Name]; ok {
			return v, nil
		}
		return nil, nil
	}
}
