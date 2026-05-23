package main

import (
	"io"
	"log"
	"strings"
	"testing"
)

// TestResolveAdvertise_BothExplicit_NoOp: when neither bind is
// unspecified, resolveAdvertise returns ("", "") and skips detection.
func TestResolveAdvertise_BothExplicit_NoOp(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	la, ca, err := resolveAdvertise("127.0.0.1:7000", "127.0.0.1:7001", lg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if la != "" || ca != "" {
		t.Errorf("got listen=%q cluster=%q; want both empty (explicit binds skip detection)", la, ca)
	}
}

// TestResolveAdvertise_Unspecified_UsesDetectedIP: when bind is
// 0.0.0.0:N, resolveAdvertise returns <detected-ip>:N. We can't fake the
// host's interfaces here, so just assert the synthesized advertise has
// the original port and is not 0.0.0.0.
func TestResolveAdvertise_Unspecified_UsesDetectedIP(t *testing.T) {
	lg := log.New(io.Discard, "", 0)
	la, ca, err := resolveAdvertise("0.0.0.0:7000", "0.0.0.0:7001", lg)
	if err != nil {
		// Acceptable on a CI runner with no usable interfaces. The error
		// must carry the operator-facing guidance.
		if !strings.Contains(err.Error(), "/etc/jaco/jacod.yaml") {
			t.Errorf("err missing operator guidance: %v", err)
		}
		return
	}
	if la == "" || ca == "" {
		t.Errorf("got listen=%q cluster=%q; want both populated", la, ca)
	}
	if strings.HasPrefix(la, "0.0.0.0") || strings.HasPrefix(ca, "0.0.0.0") {
		t.Errorf("advertise should not be 0.0.0.0; got listen=%q cluster=%q", la, ca)
	}
	if !strings.HasSuffix(la, ":7000") || !strings.HasSuffix(ca, ":7001") {
		t.Errorf("expected original ports preserved; got listen=%q cluster=%q", la, ca)
	}
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
