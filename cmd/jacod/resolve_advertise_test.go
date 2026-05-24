package main

import (
	"io"
	"log"
	"net"
	"strings"
	"testing"
)

// TestResolveAdvertise_BothExplicit_NoOp: when neither bind is
// unspecified, resolveAdvertise honors the pinned values verbatim for bind
// and leaves advertise empty (the server falls back to the bind).
func TestResolveAdvertise_BothExplicit_NoOp(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	plan, err := resolveAdvertise("127.0.0.1:7000", "127.0.0.1:7001", lg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if plan.listenAdvertise != "" || plan.clusterAdvertise != "" {
		t.Errorf("got listenAdv=%q clusterAdv=%q; want both empty (explicit binds skip detection)",
			plan.listenAdvertise, plan.clusterAdvertise)
	}
	if plan.listenBind != "127.0.0.1:7000" || plan.clusterBind != "127.0.0.1:7001" {
		t.Errorf("pinned binds must pass through verbatim; got listen=%q cluster=%q",
			plan.listenBind, plan.clusterBind)
	}
}

// TestResolveAdvertise_Unspecified_BindsDetectedIP: when bind is 0.0.0.0:N,
// resolveAdvertise resolves a private face and uses <detected-ip>:N for BOTH
// the bind and the advertise (issue #44 — the plane must not bind 0.0.0.0).
// We can't fake the host's interfaces here, so assert the synthesized
// addresses keep the original port and are never 0.0.0.0 or public.
func TestResolveAdvertise_Unspecified_BindsDetectedIP(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	plan, err := resolveAdvertise("0.0.0.0:7000", "0.0.0.0:7001", lg)
	if err != nil {
		// Acceptable on a CI runner with no usable private interfaces.
		// netdetect now refuses public-only hosts, so this path is expected
		// on hosts with only a public NIC. The error must carry guidance.
		if !strings.Contains(err.Error(), "/etc/jaco/jacod.yaml") {
			t.Errorf("err missing operator guidance: %v", err)
		}
		return
	}
	for _, tc := range []struct {
		name, bind, adv, port string
	}{
		{"listen", plan.listenBind, plan.listenAdvertise, ":7000"},
		{"cluster", plan.clusterBind, plan.clusterAdvertise, ":7001"},
	} {
		if tc.bind == "" || tc.adv == "" {
			t.Errorf("%s: got bind=%q adv=%q; want both populated", tc.name, tc.bind, tc.adv)
		}
		// Bind follows the advertise face — they must be identical.
		if tc.bind != tc.adv {
			t.Errorf("%s: bind %q != advertise %q; bind must follow advertise face", tc.name, tc.bind, tc.adv)
		}
		if strings.HasPrefix(tc.bind, "0.0.0.0") {
			t.Errorf("%s: bind should not be 0.0.0.0; got %q", tc.name, tc.bind)
		}
		if !strings.HasSuffix(tc.bind, tc.port) {
			t.Errorf("%s: expected original port preserved; got %q", tc.name, tc.bind)
		}
		// netdetect never returns a public IP, so the resolved face must be
		// private/overlay. Assert it isn't a well-known public address.
		if host, _, splitErr := net.SplitHostPort(tc.bind); splitErr == nil {
			if ip := net.ParseIP(host); ip != nil && isPublicIP(ip) {
				t.Errorf("%s: resolved bind %q is a public IP; must never be auto-picked", tc.name, tc.bind)
			}
		}
	}
}

// TestResolveAdvertise_OneExplicitOnePinned: a mixed config (explicit
// listen_addr, unspecified cluster_addr) honors the pin and resolves only
// the unspecified one. Skips cleanly when no private face exists.
func TestResolveAdvertise_OneExplicitOnePinned(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	plan, err := resolveAdvertise("10.10.0.4:7000", "0.0.0.0:7001", lg)
	if err != nil {
		if !strings.Contains(err.Error(), "/etc/jaco/jacod.yaml") {
			t.Errorf("err missing operator guidance: %v", err)
		}
		return
	}
	if plan.listenBind != "10.10.0.4:7000" {
		t.Errorf("pinned listen bind must pass through; got %q", plan.listenBind)
	}
	if plan.listenAdvertise != "" {
		t.Errorf("pinned listen advertise should stay empty (server falls back to bind); got %q", plan.listenAdvertise)
	}
	if plan.clusterBind == "" || plan.clusterBind == "0.0.0.0:7001" {
		t.Errorf("unspecified cluster bind must resolve to a real face; got %q", plan.clusterBind)
	}
	if plan.clusterBind != plan.clusterAdvertise {
		t.Errorf("cluster bind %q must follow advertise %q", plan.clusterBind, plan.clusterAdvertise)
	}
}

// isPublicIP reports whether ip is a globally-routable public IPv4 — the
// inverse of the private/overlay/special ranges netdetect is allowed to
// pick. Used by tests to assert the resolver never lands on a public face.
func isPublicIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false // out of scope for v0; treat as non-public
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"} {
		if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(v4) {
			return false
		}
	}
	return true
}

// TestSplitUnspecified_Variants covers the helper directly.
func TestSplitUnspecified_Variants(t *testing.T) {
	cases := []struct {
		in        string
		wantUnspc bool
		wantPort  string
		wantErr   bool
	}{
		{"0.0.0.0:7000", true, "7000", false},
		{"[::]:7000", true, "7000", false},
		{"127.0.0.1:7000", false, "7000", false},
		{"10.0.0.5:7000", false, "7000", false},
		{"jaco-1:7000", false, "7000", false}, // hostname literal — explicit
		{"", false, "", false},
		{"no-port", false, "", true},
	}
	for _, c := range cases {
		got, port, err := splitUnspecified(c.in, "test_field")
		if (err != nil) != c.wantErr {
			t.Errorf("splitUnspecified(%q): err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if got != c.wantUnspc || port != c.wantPort {
			t.Errorf("splitUnspecified(%q) = (%v, %q); want (%v, %q)", c.in, got, port, c.wantUnspc, c.wantPort)
		}
	}
}
