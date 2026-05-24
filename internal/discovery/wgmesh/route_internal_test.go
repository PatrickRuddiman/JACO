package wgmesh

import (
	"reflect"
	"testing"
)

func TestRouteDiff(t *testing.T) {
	desired := []string{"10.244.5.0/24", "10.244.6.0/24"}
	current := []string{"10.244.6.0/24", "10.244.9.0/24"}

	add, del := routeDiff(desired, current)
	if !reflect.DeepEqual(add, []string{"10.244.5.0/24"}) {
		t.Errorf("add = %v, want [10.244.5.0/24]", add)
	}
	if !reflect.DeepEqual(del, []string{"10.244.9.0/24"}) {
		t.Errorf("del = %v, want [10.244.9.0/24]", del)
	}
}

func TestRouteDiff_NoChange(t *testing.T) {
	routes := []string{"10.244.5.0/24"}
	add, del := routeDiff(routes, routes)
	if len(add) != 0 || len(del) != 0 {
		t.Errorf("add=%v del=%v, want both empty", add, del)
	}
}

func TestParseRouteCIDRs(t *testing.T) {
	// Realistic `ip route show dev jaco0` output: container /24s plus a
	// scope-link line and a blank line that must be ignored.
	out := `10.244.5.0/24 scope link
10.244.6.0/24 proto static

10.99.0.0/24 scope link
broadcast`
	got := parseRouteCIDRs(out)
	want := []string{"10.244.5.0/24", "10.244.6.0/24", "10.99.0.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseRouteCIDRs = %v, want %v", got, want)
	}
}
