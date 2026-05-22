package placement_test

import (
	"errors"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/placement"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func nodes(ready ...string) []*pb.Node {
	out := make([]*pb.Node, 0, len(ready))
	for _, h := range ready {
		out = append(out, &pb.Node{Hostname: h, Status: pb.NodeStatus_NODE_STATUS_READY})
	}
	return out
}

func TestEligibleHosts_FiltersByReadyStatus(t *testing.T) {
	all := []*pb.Node{
		{Hostname: "node-a", Status: pb.NodeStatus_NODE_STATUS_READY},
		{Hostname: "node-b", Status: pb.NodeStatus_NODE_STATUS_JOINING},
		{Hostname: "node-c", Status: pb.NodeStatus_NODE_STATUS_READY},
		{Hostname: "node-d", Status: pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE},
	}
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}
	got := placement.EligibleHosts(spec, all)
	want := []string{"node-a", "node-c"}
	if !equalStrings(got, want) {
		t.Errorf("EligibleHosts = %v, want %v", got, want)
	}
}

func TestEligibleHosts_HostsModeIntersectsWithSpecHosts(t *testing.T) {
	all := nodes("node-a", "node-b", "node-c")
	spec := &pb.ServiceSpec{
		Name:      "web",
		Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
		Hosts:     []string{"node-b", "node-c", "node-z"}, // node-z unhealthy/missing
	}
	got := placement.EligibleHosts(spec, all)
	want := []string{"node-b", "node-c"}
	if !equalStrings(got, want) {
		t.Errorf("EligibleHosts = %v, want %v", got, want)
	}
}

func TestPlaceReplica_NoEligibleHostsErrors(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Replicas: 1, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}
	_, err := placement.PlaceReplica("sample", spec, nil, 0, nil)
	if err == nil {
		t.Fatal("expected error on empty eligible set")
	}
	var pe *placement.PlacementError
	if !errors.As(err, &pe) || pe.Code != "cannot_satisfy_host_placement" {
		t.Errorf("err = %v, want cannot_satisfy_host_placement", err)
	}
}

func TestPlaceReplica_HostsModeFailsWhenInsufficient(t *testing.T) {
	spec := &pb.ServiceSpec{
		Name:      "web",
		Replicas:  3,
		Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
	}
	eligible := []string{"node-a", "node-b"} // only 2 of 3 wanted
	_, err := placement.PlaceReplica("sample", spec, eligible, 0, nil)
	var pe *placement.PlacementError
	if !errors.As(err, &pe) || pe.Code != "cannot_satisfy_host_placement" {
		t.Fatalf("err = %v, want cannot_satisfy_host_placement", err)
	}
	if pe.Details["requested"] != "3" || pe.Details["eligible"] != "2" {
		t.Errorf("details = %+v", pe.Details)
	}
}

func TestPlaceReplica_HostsModeAssignsOneReplicaPerPinnedHost(t *testing.T) {
	// HOSTS mode requires len(eligible) >= replicas — the slice §3 rule. With
	// 2 replicas across 2 pinned hosts, each replica lands on a distinct host
	// by index.
	spec := &pb.ServiceSpec{
		Name:      "web",
		Replicas:  2,
		Placement: pb.ServiceSpec_PLACEMENT_MODE_HOSTS,
	}
	eligible := []string{"node-a", "node-b"}
	hosts := make([]string, 2)
	for i := 0; i < 2; i++ {
		h, err := placement.PlaceReplica("sample", spec, eligible, i, nil)
		if err != nil {
			t.Fatal(err)
		}
		hosts[i] = h
	}
	want := []string{"node-a", "node-b"}
	if !equalStrings(hosts, want) {
		t.Errorf("HOSTS placement = %v, want %v", hosts, want)
	}
}

func TestPlaceReplica_SpreadDeterministic1000Calls(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}
	eligible := []string{"node-a", "node-b", "node-c"}
	first, _ := placement.PlaceReplica("sample", spec, eligible, 0, nil)
	for i := 0; i < 1000; i++ {
		got, err := placement.PlaceReplica("sample", spec, eligible, 0, nil)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("call %d: got %s, want %s — must be deterministic", i, got, first)
		}
	}
}

func TestPlaceReplica_SpreadDistributesEvenly(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}
	eligible := []string{"node-a", "node-b", "node-c"}
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		h, err := placement.PlaceReplica("sample", spec, eligible, i, nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[h]++
	}
	for h, c := range counts {
		if c < 2 || c > 4 {
			t.Errorf("host %s got %d replicas; want within 1 of 9/3=3 (i.e. 2..4)", h, c)
		}
	}
	if got := len(counts); got != 3 {
		t.Errorf("hosts used = %d, want 3", got)
	}
}

func TestPlaceReplica_SpreadStableUnderEligibleOrderingShuffle(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}
	a := []string{"node-a", "node-b", "node-c"}
	b := []string{"node-c", "node-a", "node-b"} // same set, different order
	for i := 0; i < 5; i++ {
		hA, _ := placement.PlaceReplica("sample", spec, a, i, nil)
		hB, _ := placement.PlaceReplica("sample", spec, b, i, nil)
		if hA != hB {
			t.Errorf("index %d: a→%s, b→%s — order should not affect result", i, hA, hB)
		}
	}
}

func TestPlaceReplica_PackPicksLeastLoadedHost(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_PACK}
	eligible := []string{"node-a", "node-b", "node-c"}
	counts := map[string]int{"node-a": 5, "node-b": 1, "node-c": 3}
	got, err := placement.PlaceReplica("sample", spec, eligible, 0, counts)
	if err != nil {
		t.Fatal(err)
	}
	if got != "node-b" {
		t.Errorf("PACK pick = %s, want node-b (least-loaded)", got)
	}
}

func TestPlaceReplica_PackTiebreakerHostnameLex(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_PACK}
	eligible := []string{"node-a", "node-b", "node-c"}
	counts := map[string]int{"node-a": 2, "node-b": 2, "node-c": 2}
	got, err := placement.PlaceReplica("sample", spec, eligible, 0, counts)
	if err != nil {
		t.Fatal(err)
	}
	if got != "node-a" {
		t.Errorf("PACK tiebreak = %s, want node-a", got)
	}
}

func TestPlaceReplica_PackMissingCountsTreatedAsZero(t *testing.T) {
	spec := &pb.ServiceSpec{Name: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_PACK}
	eligible := []string{"node-a", "node-b", "node-c"}
	counts := map[string]int{"node-a": 2, "node-c": 1}
	got, err := placement.PlaceReplica("sample", spec, eligible, 0, counts)
	if err != nil {
		t.Fatal(err)
	}
	if got != "node-b" {
		t.Errorf("PACK with missing counts = %s, want node-b (treated as 0)", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
