package firewall

import (
	"reflect"
	"testing"
)

func TestOverlayAcceptRule(t *testing.T) {
	got := overlayAcceptRule("10.244.0.0/16")
	want := []string{"-s", "10.244.0.0/16", "-d", "10.244.0.0/16", "-j", "ACCEPT"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("overlayAcceptRule = %v, want %v", got, want)
	}
}

func TestConcatArgs(t *testing.T) {
	// Builds a fresh slice from groups; nil groups are skipped.
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
	if got := overlayWant("DOCKER-USER", "10.244.0.0/16"); got != "-A DOCKER-USER -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT" {
		t.Errorf("overlayWant = %q", got)
	}
}

func TestFirstAppendedRuleIs(t *testing.T) {
	pool := "10.244.0.0/16"
	wantRaw := overlayWant("PREROUTING", pool)
	wantDU := overlayWant("DOCKER-USER", pool)

	cases := []struct {
		name   string
		output string
		chain  string
		want   string
		expect bool
	}{
		{
			name:   "accept is first appended rule",
			output: "-P PREROUTING ACCEPT\n-A PREROUTING -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT\n-A PREROUTING -d 10.244.1.2 ! -i jaco-b26d-b26d -j DROP",
			chain:  "PREROUTING",
			want:   wantRaw,
			expect: true,
		},
		{
			name:   "drifted below a docker drop",
			output: "-P PREROUTING ACCEPT\n-A PREROUTING -d 10.244.1.2 ! -i jaco-b26d-b26d -j DROP\n-A PREROUTING -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT",
			chain:  "PREROUTING",
			want:   wantRaw,
			expect: false,
		},
		{
			name:   "absent entirely",
			output: "-P PREROUTING ACCEPT\n-A PREROUTING -d 10.244.1.2 ! -i jaco-b26d-b26d -j DROP",
			chain:  "PREROUTING",
			want:   wantRaw,
			expect: false,
		},
		{
			name:   "empty docker-user chain",
			output: "-N DOCKER-USER",
			chain:  "DOCKER-USER",
			want:   wantDU,
			expect: false,
		},
		{
			name:   "docker-user accept is first",
			output: "-N DOCKER-USER\n-A DOCKER-USER -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT",
			chain:  "DOCKER-USER",
			want:   wantDU,
			expect: true,
		},
		{
			name:   "ignores unrelated chains before the target",
			output: "-A FORWARD -j DOCKER-USER\n-A DOCKER-USER -s 10.244.0.0/16 -d 10.244.0.0/16 -j ACCEPT",
			chain:  "DOCKER-USER",
			want:   wantDU,
			expect: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstAppendedRuleIs(tc.output, tc.chain, tc.want); got != tc.expect {
				t.Errorf("firstAppendedRuleIs = %v, want %v", got, tc.expect)
			}
		})
	}
}
