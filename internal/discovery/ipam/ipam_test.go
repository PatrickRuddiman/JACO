package ipam_test

import (
	"fmt"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/ipam"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newHarness(t *testing.T) (*ipam.IPAM, *state.State) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	applier := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	i, err := ipam.New(st, applier, ipam.DefaultPoolCIDR)
	if err != nil {
		t.Fatalf("ipam.New: %v", err)
	}
	return i, st
}

func TestNew_RejectsNon16Pool(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	apply := func([]byte) error { return nil }

	cases := []string{"10.244.0.0/24", "10.244.0.0/12", "garbage"}
	for _, c := range cases {
		if _, err := ipam.New(st, apply, c); err == nil {
			t.Errorf("New(%q) accepted bad pool", c)
		}
	}
}

func TestAllocate_ReturnsExistingForSameKey(t *testing.T) {
	i, _ := newHarness(t)
	first, err := i.Allocate("sample", "frontend")
	if err != nil {
		t.Fatal(err)
	}
	second, err := i.Allocate("sample", "frontend")
	if err != nil {
		t.Fatal(err)
	}
	if first.GetCidr() != second.GetCidr() {
		t.Errorf("Allocate not idempotent: %s vs %s", first.GetCidr(), second.GetCidr())
	}
}

func TestAllocate_HundredAllocationsAreUnique(t *testing.T) {
	i, _ := newHarness(t)
	seen := make(map[string]string)
	for n := 0; n < 100; n++ {
		dep := fmt.Sprintf("dep-%d", n)
		s, err := i.Allocate(dep, "default")
		if err != nil {
			t.Fatalf("alloc %d: %v", n, err)
		}
		cidr := s.GetCidr()
		if prev, ok := seen[cidr]; ok {
			t.Fatalf("duplicate CIDR %s: %s and %s", cidr, prev, dep)
		}
		seen[cidr] = dep
	}
	if got := len(seen); got != 100 {
		t.Errorf("distinct CIDRs = %d, want 100", got)
	}
}

func TestAllocate_PoolExhaustionReturnsTypedError(t *testing.T) {
	i, _ := newHarness(t)
	// Fill all 256 /24 slots.
	for n := 0; n < 256; n++ {
		dep := fmt.Sprintf("dep-%d", n)
		if _, err := i.Allocate(dep, "default"); err != nil {
			t.Fatalf("alloc %d: %v", n, err)
		}
	}
	// The 257th request must fail with ipam_pool_exhausted.
	_, err := i.Allocate("overflow", "default")
	if err == nil {
		t.Fatal("expected ipam_pool_exhausted error")
	}
	if !ipam.IsExhausted(err) {
		t.Errorf("err = %v; want ipam_pool_exhausted", err)
	}
}

func TestFree_ReleasesSlotForReuse(t *testing.T) {
	i, _ := newHarness(t)

	// Fill the first three slots.
	a, _ := i.Allocate("a", "default") // expected 10.244.0.0/24
	b, _ := i.Allocate("b", "default") // expected 10.244.1.0/24
	c, _ := i.Allocate("c", "default") // expected 10.244.2.0/24

	if a.GetCidr() == "" || b.GetCidr() == "" || c.GetCidr() == "" {
		t.Fatalf("missing cidrs: %v %v %v", a, b, c)
	}

	// Free the middle slot.
	if err := i.Free("b", "default"); err != nil {
		t.Fatalf("Free: %v", err)
	}

	// Next allocation lands on the freed slot (the lowest-numbered free
	// /24 is now b's old CIDR).
	d, _ := i.Allocate("d", "default")
	if d.GetCidr() != b.GetCidr() {
		t.Errorf("new alloc CIDR = %s, want freed %s", d.GetCidr(), b.GetCidr())
	}
}

func TestFree_NoOpWhenMissing(t *testing.T) {
	i, _ := newHarness(t)
	if err := i.Free("ghost", "default"); err != nil {
		t.Errorf("Free on missing should no-op; got %v", err)
	}
}

func TestEnsureSubnets_AllocatesAllNetworks(t *testing.T) {
	i, _ := newHarness(t)
	out, err := i.EnsureSubnets("sample", []string{"frontend", "backend", "metrics"})
	if err != nil {
		t.Fatalf("EnsureSubnets: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d subnets, want 3", len(out))
	}
	seen := map[string]bool{}
	for _, s := range out {
		if seen[s.GetCidr()] {
			t.Errorf("duplicate cidr in EnsureSubnets output: %s", s.GetCidr())
		}
		seen[s.GetCidr()] = true
	}
}

func TestEnsureSubnets_IdempotentReturnsSameCIDRs(t *testing.T) {
	i, _ := newHarness(t)
	first, _ := i.EnsureSubnets("sample", []string{"frontend", "backend"})
	second, _ := i.EnsureSubnets("sample", []string{"frontend", "backend"})
	if len(first) != len(second) {
		t.Fatalf("len: first=%d second=%d", len(first), len(second))
	}
	for idx := range first {
		if first[idx].GetCidr() != second[idx].GetCidr() {
			t.Errorf("idx %d cidr drift: %s -> %s", idx, first[idx].GetCidr(), second[idx].GetCidr())
		}
	}
}

func TestAllocate_RejectsEmptyArgs(t *testing.T) {
	i, _ := newHarness(t)
	if _, err := i.Allocate("", "default"); err == nil {
		t.Errorf("empty deployment accepted")
	}
	if _, err := i.Allocate("sample", ""); err == nil {
		t.Errorf("empty network accepted")
	}
}

func TestAllocate_WritesPersistedSubnet(t *testing.T) {
	i, st := newHarness(t)
	s, err := i.Allocate("sample", "frontend")
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := st.Subnets.Get(state.SubnetKey("sample", "frontend"))
	if !ok {
		t.Fatalf("Subnet not persisted to state")
	}
	if stored.GetCidr() != s.GetCidr() {
		t.Errorf("returned %s but state has %s", s.GetCidr(), stored.GetCidr())
	}
	if stored.GetDeployment() != "sample" || stored.GetNetwork() != "frontend" {
		t.Errorf("scope wrong: %+v", stored)
	}
}

// silence unused import in case the test file shrinks
var _ pb.Subnet
