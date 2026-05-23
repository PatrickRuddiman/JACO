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

func TestPick_TailscaleByName_BeatsLAN(t *testing.T) {
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
	if ip.String() != "100.96.1.2" || name != "tailscale0" {
		t.Errorf("got %s on %s; want 100.96.1.2 on tailscale0", ip, name)
	}
}

func TestPick_TailscaleByCGNATRange_BeatsLAN(t *testing.T) {
	// Interface NOT named tailscale*, but IP is in 100.64/10 — still class 1.
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
	if ip.String() != "100.127.5.5" || name != "eth1" {
		t.Errorf("got %s on %s; want 100.127.5.5 on eth1", ip, name)
	}
}

func TestPick_TunBeatsRFC1918(t *testing.T) {
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp),
		mkIface("wg0", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("10.0.0.5"),
		"wg0":  mkAddrs("10.244.1.1"), // also RFC1918, but wg0 is class 2
	}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if name != "wg0" {
		t.Errorf("got %s on %s; want wg0", ip, name)
	}
}

func TestPick_RFC1918BeatsPublic(t *testing.T) {
	ifaces := []net.Interface{
		mkIface("eth0", net.FlagUp),
		mkIface("eth1", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mkAddrs("8.8.8.8"),
		"eth1": mkAddrs("172.20.5.5"),
	}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ip.String() != "172.20.5.5" || name != "eth1" {
		t.Errorf("got %s on %s; want 172.20.5.5 on eth1", ip, name)
	}
}

func TestPick_PublicOnly_FallsThroughToClass4(t *testing.T) {
	ifaces := []net.Interface{mkIface("eth0", net.FlagUp)}
	addrs := map[string][]net.Addr{"eth0": mkAddrs("198.51.100.7")}
	ip, name, err := pickFromInterfaces(ifaces, lookup(addrs))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if ip.String() != "198.51.100.7" || name != "eth0" {
		t.Errorf("got %s on %s; want 198.51.100.7 on eth0", ip, name)
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
		{"tailscale0", "100.96.1.2", 1},
		{"eth0", "100.127.5.5", 1}, // CGNAT regardless of iface name
		{"wg0", "10.244.0.1", 2},
		{"tun0", "10.0.0.1", 2},
		{"tap0", "192.168.1.1", 2},
		{"jaco0", "10.0.0.99", 2},
		{"eth0", "10.0.0.5", 3},
		{"eth0", "172.20.0.5", 3},
		{"eth0", "192.168.1.5", 3},
		{"eth0", "8.8.8.8", 4},
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
