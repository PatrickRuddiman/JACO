package firewall

import (
	"reflect"
	"testing"
)

func TestOverlayAcceptRule(t *testing.T) {
	got := overlayAcceptRule("10.99.0.0/24", "10.244.0.0/16")
	want := []string{"-s", "10.99.0.0/24", "-d", "10.244.0.0/16", "-j", "ACCEPT"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("overlayAcceptRule = %v, want %v", got, want)
	}
}

func TestConcatArgs(t *testing.T) {
	got := concatArgs([]string{"-t", "raw"}, []string{"-I", "PREROUTING", "1"}, nil)
	want := []string{"-t", "raw", "-I", "PREROUTING", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("concatArgs = %v, want %v", got, want)
	}
	// No aliasing: two builds off the same prefix must not clobber each other.
	prefix := []string{"-t", "raw"}
	a := concatArgs(prefix, []string{"-D", "PREROUTING"}, nil)
	b := concatArgs(prefix, []string{"-I", "PREROUTING", "1"}, nil)
	if a[2] != "-D" || b[2] != "-I" {
		t.Errorf("concatArgs aliased backing array: a=%v b=%v", a, b)
	}
}

func TestOverlayWant(t *testing.T) {
	if got := overlayWant("DOCKER-USER", "10.244.0.0/16", "10.244.0.0/16"); got != "-A DOCKER-USER -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT" {
		t.Errorf("overlayWant = %q", got)
	}
}

func TestTopAppendedRulesMatch(t *testing.T) {
	pool, mesh := "10.244.0.0/16", "10.99.0.0/24"
	wants := []string{
		overlayWant("PREROUTING", pool, pool),
		overlayWant("PREROUTING", mesh, pool),
	}
	cases := []struct {
		name   string
		output string
		expect bool
	}{
		{
			name: "both at top in order",
			output: "-P PREROUTING ACCEPT\n" +
				"-A PREROUTING -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT\n" +
				"-A PREROUTING -s 10.99.0.0/24 -d 10.244.0.0/16 -j ACCEPT\n" +
				"-A PREROUTING -d 10.244.1.2/32 ! -i jaco-b26d-b26d -j DROP",
			expect: true,
		},
		{
			name: "wrong order",
			output: "-A PREROUTING -s 10.99.0.0/24 -d 10.244.0.0/16 -j ACCEPT\n" +
				"-A PREROUTING -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT",
			expect: false,
		},
		{
			name: "drifted below a docker drop",
			output: "-A PREROUTING -d 10.244.1.2/32 ! -i jaco-b26d-b26d -j DROP\n" +
				"-A PREROUTING -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT\n" +
				"-A PREROUTING -s 10.99.0.0/24 -d 10.244.0.0/16 -j ACCEPT",
			expect: false,
		},
		{
			name:   "only one present",
			output: "-A PREROUTING -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT",
			expect: false,
		},
		{
			name:   "empty chain",
			output: "-P PREROUTING ACCEPT",
			expect: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := topAppendedRulesMatch(tc.output, "PREROUTING", wants); got != tc.expect {
				t.Errorf("topAppendedRulesMatch = %v, want %v", got, tc.expect)
			}
		})
	}
}
