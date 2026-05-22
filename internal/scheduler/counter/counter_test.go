package counter_test

import (
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/counter"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// applierFn wires Counter.apply through to the real FSM so the state store
// reflects the increment — tests can then read state.ReplicaCounters to
// verify monotonicity.
func applierFn(f *fsm.FSM, index *uint64) counter.Applier {
	return func(data []byte) error {
		*index++
		f.Apply(&hraft.Log{Index: *index, Data: data})
		return nil
	}
}

func newCounter(t *testing.T) (*counter.Counter, *state.State, *fsm.FSM) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIndex uint64
	c := counter.New(st, applierFn(f, &raftIndex))
	return c, st, f
}

func TestNext_StartsAtOne(t *testing.T) {
	c, _, _ := newCounter(t)
	got, err := c.Next("sample", "web")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != 1 {
		t.Errorf("first Next = %d, want 1", got)
	}
}

func TestNext_MonotonicAcross100Calls(t *testing.T) {
	c, _, _ := newCounter(t)
	seen := map[uint64]bool{}
	var last uint64
	for i := 0; i < 100; i++ {
		got, err := c.Next("sample", "web")
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if seen[got] {
			t.Fatalf("Next returned a duplicate index %d on call %d", got, i)
		}
		seen[got] = true
		if got <= last {
			t.Fatalf("Next returned %d after %d — not monotonic", got, last)
		}
		last = got
	}
	if len(seen) != 100 {
		t.Errorf("distinct indices = %d, want 100", len(seen))
	}
}

func TestNext_NeverReusesAfterDeploymentDelete(t *testing.T) {
	c, st, f := newCounter(t)

	// Issue 3 replica ids.
	for i := 0; i < 3; i++ {
		if _, err := c.Next("sample", "web"); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate a Deploy.Delete (the FSM cascade removes Routes + Replicas
	// but ReplicaCounters survives so indices never get reused).
	deleteCmd := &pb.Command{
		Identity: "operator",
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_DeploymentDelete{DeploymentDelete: &pb.DeploymentDelete{Deployment: "sample"}},
	}
	data, _ := proto.Marshal(deleteCmd)
	f.Apply(&hraft.Log{Index: 100, Data: data})

	// The counter must remember it has handed out 1, 2, 3 — Next returns 4.
	got, err := c.Next("sample", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("Next after Delete = %d, want 4 (no reuse)", got)
	}

	// And the on-disk counter agrees.
	rc, ok := st.ReplicaCounters.Get(state.ReplicaCounterKey("sample", "web"))
	if !ok {
		t.Fatalf("ReplicaCounter missing post-Delete")
	}
	if rc.GetNextIndex() != 4 {
		t.Errorf("ReplicaCounter.next_index = %d, want 4", rc.GetNextIndex())
	}
}

func TestNext_DistinctServicesHaveIndependentCounters(t *testing.T) {
	c, _, _ := newCounter(t)
	if _, err := c.Next("sample", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next("sample", "web"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Next("sample", "api")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("Next(sample/api) = %d, want 1 (independent counter)", got)
	}
	got, err = c.Next("sample", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("Next(sample/web) third = %d, want 3", got)
	}
}

func TestNext_RejectsEmptyArgs(t *testing.T) {
	c, _, _ := newCounter(t)
	if _, err := c.Next("", "web"); err == nil {
		t.Error("empty deployment should error")
	}
	if _, err := c.Next("sample", ""); err == nil {
		t.Error("empty service should error")
	}
}

func TestReplicaID_Format(t *testing.T) {
	cases := []struct {
		dep, svc string
		idx      uint64
		want     string
	}{
		{"sample", "web", 0, "sample-web-0"},
		{"sample", "web", 42, "sample-web-42"},
		{"front", "api", 1, "front-api-1"},
	}
	for _, c := range cases {
		got := counter.ReplicaID(c.dep, c.svc, c.idx)
		if got != c.want {
			t.Errorf("ReplicaID(%q,%q,%d) = %q, want %q", c.dep, c.svc, c.idx, got, c.want)
		}
	}
}
