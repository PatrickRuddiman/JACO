package rebalance_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// newStateWithNode seeds a state.State with a single Node carrying
// the supplied pressure sample + LastPressureAt. Idx is 1 because it's
// the only Apply; callers don't need to thread real raft indices.
func newStateWithNode(host string, cpu, mem float64, lastAt time.Time) *state.State {
	st := state.New(watch.NewRegistry())
	n := &pb.Node{
		Hostname:       host,
		Status:         pb.NodeStatus_NODE_STATUS_READY,
		CpuPressure:    cpu,
		MemoryPressure: mem,
		LastPressureAt: timestamppb.New(lastAt),
	}
	st.Nodes.Apply(n, 1)
	return st
}

// TestStateBackedSource_FreshSampleOk — a node with a recent
// LastPressureAt returns its gossiped (CPU, Memory) verbatim.
func TestStateBackedSource_FreshSampleOk(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := newStateWithNode("jaco-1", 0.7, 0.4, now.Add(-10*time.Second))
	src := &rebalance.StateBackedSource{
		State:  st,
		MaxAge: 90 * time.Second,
		Now:    func() time.Time { return now },
	}
	got, ok := src.NodePressure("jaco-1")
	if !ok {
		t.Fatalf("want ok=true, got !ok")
	}
	if got.CPU != 0.7 || got.Memory != 0.4 {
		t.Errorf("snapshot = %+v; want CPU=0.7 Memory=0.4", got)
	}
}

// TestStateBackedSource_StaleSampleNotOk — a sample older than MaxAge
// is treated as missing, so the rebalancer skips the node this cycle.
func TestStateBackedSource_StaleSampleNotOk(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := newStateWithNode("jaco-2", 0.9, 0.9, now.Add(-5*time.Minute))
	src := &rebalance.StateBackedSource{
		State:  st,
		MaxAge: 90 * time.Second,
		Now:    func() time.Time { return now },
	}
	if _, ok := src.NodePressure("jaco-2"); ok {
		t.Errorf("stale sample must be !ok")
	}
}

// TestStateBackedSource_UnknownHostNotOk — a host not in state.Nodes
// is !ok rather than a synthetic zero snapshot.
func TestStateBackedSource_UnknownHostNotOk(t *testing.T) {
	st := state.New(watch.NewRegistry())
	src := &rebalance.StateBackedSource{State: st, MaxAge: 90 * time.Second}
	if _, ok := src.NodePressure("ghost"); ok {
		t.Errorf("unknown host must be !ok")
	}
}

// TestStateBackedSource_MissingLastPressureAtNotOk — a node that has
// never gossiped a sample (LastPressureAt nil) is !ok even though
// CpuPressure / MemoryPressure read as zero.
func TestStateBackedSource_MissingLastPressureAtNotOk(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Nodes.Apply(&pb.Node{Hostname: "jaco-3", Status: pb.NodeStatus_NODE_STATUS_READY}, 1)
	src := &rebalance.StateBackedSource{State: st, MaxAge: 90 * time.Second}
	if _, ok := src.NodePressure("jaco-3"); ok {
		t.Errorf("fresh joiner with no sample must be !ok")
	}
}

// TestStateBackedSource_ReplicaFootprintDefaultWhenUnwired — without
// a FootprintFor closure, the source returns the tiny default that
// won't trigger moves on its own.
func TestStateBackedSource_ReplicaFootprintDefaultWhenUnwired(t *testing.T) {
	src := &rebalance.StateBackedSource{}
	fp := src.ReplicaFootprint("any")
	if fp.CPU == 0 || fp.Memory == 0 {
		t.Errorf("default footprint must be non-zero, got %+v", fp)
	}
	if fp.CPU > 0.2 || fp.Memory > 0.2 {
		t.Errorf("default footprint must be small (<0.2), got %+v", fp)
	}
}

// TestStateBackedSource_ReplicaFootprintCallsClosure — when wired, the
// closure value flows through unchanged.
func TestStateBackedSource_ReplicaFootprintCallsClosure(t *testing.T) {
	src := &rebalance.StateBackedSource{
		FootprintFor: func(id string) (float64, float64) {
			if id == "web-0" {
				return 0.3, 0.15
			}
			return 0, 0
		},
	}
	fp := src.ReplicaFootprint("web-0")
	if fp.CPU != 0.3 || fp.Memory != 0.15 {
		t.Errorf("footprint = %+v; want CPU=0.3 Memory=0.15", fp)
	}
	// Closure returning (0,0) falls through to default.
	fp = src.ReplicaFootprint("other")
	if fp.CPU == 0 {
		t.Errorf("unknown replica should fall back to default; got %+v", fp)
	}
}

// TestStateBackedSource_NoMaxAgeMeansAlwaysFresh — MaxAge=0 disables
// the freshness check (every sample with a non-nil LastPressureAt is
// considered current). Useful in tests; not used in production.
func TestStateBackedSource_NoMaxAgeMeansAlwaysFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := newStateWithNode("jaco-1", 0.5, 0.5, now.Add(-time.Hour))
	src := &rebalance.StateBackedSource{
		State: st,
		Now:   func() time.Time { return now },
	}
	if _, ok := src.NodePressure("jaco-1"); !ok {
		t.Errorf("MaxAge=0 should accept any non-nil timestamp")
	}
}
