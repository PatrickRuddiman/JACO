package wgmesh

import (
	"reflect"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
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

func TestLocalPoolGateway(t *testing.T) {
	st := state.New(watch.NewRegistry())
	// Two local subnets + one remote. Gateway must come from the
	// lexicographically-first LOCAL CIDR (10.244.2.0/24 → .1), never the
	// remote one.
	st.Subnets.Apply(&pb.Subnet{Deployment: "app", Network: "n", Cidr: "10.244.5.0/24", Host: "self"}, 1)
	st.Subnets.Apply(&pb.Subnet{Deployment: "app", Network: "m", Cidr: "10.244.2.0/24", Host: "self"}, 2)
	st.Subnets.Apply(&pb.Subnet{Deployment: "app", Network: "p", Cidr: "10.244.9.0/24", Host: "other"}, 3)

	s := &Syncer{State: st, SelfHostname: "self"}
	if got := s.localPoolGateway(); got != "10.244.2.1" {
		t.Errorf("localPoolGateway = %q, want 10.244.2.1", got)
	}
}

func TestLocalPoolGateway_NoLocalSubnet(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Subnets.Apply(&pb.Subnet{Deployment: "app", Network: "n", Cidr: "10.244.5.0/24", Host: "other"}, 1)
	s := &Syncer{State: st, SelfHostname: "self"}
	if got := s.localPoolGateway(); got != "" {
		t.Errorf("localPoolGateway = %q, want empty (no local subnet)", got)
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
