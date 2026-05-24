package grpc

import (
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestPurgeHostlessSubnets(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	// Start the apply index above the seed indices so FSM removals aren't
	// rejected by the store's monotonic-index guard.
	idx := uint64(10)
	apply := func(data []byte) error {
		idx++
		f.Apply(&hraft.Log{Index: idx, Data: data})
		return nil
	}

	// Two pre-#28 host-less entries + one host-bearing entry.
	st.Subnets.Apply(&pb.Subnet{Deployment: "a", Network: "_default", Cidr: "10.244.0.0/24"}, 1)
	st.Subnets.Apply(&pb.Subnet{Deployment: "b", Network: "backend", Cidr: "10.244.1.0/24"}, 2)
	st.Subnets.Apply(&pb.Subnet{Deployment: "c", Network: "_default", Cidr: "10.244.2.0/24", Host: "host-a"}, 3)

	n, err := purgeHostlessSubnets(st, apply)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("purged = %d, want 2", n)
	}
	if _, ok := st.Subnets.Get(state.SubnetKey("a", "_default", "")); ok {
		t.Error("host-less a/_default should be purged")
	}
	if _, ok := st.Subnets.Get(state.SubnetKey("b", "backend", "")); ok {
		t.Error("host-less b/backend should be purged")
	}
	if _, ok := st.Subnets.Get(state.SubnetKey("c", "_default", "host-a")); !ok {
		t.Error("host-bearing c/_default/host-a must survive the migration")
	}
}

func TestPurgeHostlessSubnets_NoneWhenAllHaveHost(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	apply := func([]byte) error { return nil }
	st.Subnets.Apply(&pb.Subnet{Deployment: "a", Network: "_default", Cidr: "10.244.0.0/24", Host: "host-a"}, 1)

	n, err := purgeHostlessSubnets(st, apply)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("purged = %d, want 0 (nothing host-less)", n)
	}
}
