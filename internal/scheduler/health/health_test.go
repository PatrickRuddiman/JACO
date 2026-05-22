package health_test

import (
	"sync/atomic"
	"testing"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/health"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeLeader matches the pattern from the other scheduler tests.
type fakeLeader struct{ leader bool }

func (f *fakeLeader) IsLeader() bool { return f.leader }

// recordingApplier wraps the FSM apply path AND tallies which Command
// variant landed so tests can assert "no restart was emitted after the
// third failure" by counting ReplicaCommandIssue{op:restart}.
type recordingApplier struct {
	f          *fsm.FSM
	raftIdx    *uint64
	restartIss int64 // total ReplicaCommandIssue{op:restart} written
	exhausted  int64 // total ReplicaObservedUpdate{code:restart_exhausted}
	counterInc int64 // total RestartCounterUpdate{INCREMENT}
	counterRst int64 // total RestartCounterUpdate{RESET}
	removeRout int64 // total ReplicaCommandIssue{op:remove_from_routing}
}

func newRecordingApplier(f *fsm.FSM, raftIdx *uint64) *recordingApplier {
	return &recordingApplier{f: f, raftIdx: raftIdx}
}

func (r *recordingApplier) Apply(data []byte) error {
	*r.raftIdx++
	var cmd pb.Command
	_ = proto.Unmarshal(data, &cmd)
	r.tally(&cmd)
	r.f.Apply(&hraft.Log{Index: *r.raftIdx, Data: data})
	return nil
}

func (r *recordingApplier) tally(cmd *pb.Command) {
	switch p := cmd.GetPayload().(type) {
	case *pb.Command_Batch:
		for _, child := range p.Batch.GetChildren() {
			r.tally(child)
		}
	case *pb.Command_ReplicaCommandIssue:
		if p.ReplicaCommandIssue.GetOp() == "restart" {
			atomic.AddInt64(&r.restartIss, 1)
		}
		if p.ReplicaCommandIssue.GetOp() == "remove_from_routing" {
			atomic.AddInt64(&r.removeRout, 1)
		}
	case *pb.Command_ReplicaObservedUpdate:
		if p.ReplicaObservedUpdate.GetReplica().GetCode() == "restart_exhausted" {
			atomic.AddInt64(&r.exhausted, 1)
		}
	case *pb.Command_RestartCounterUpdate:
		switch p.RestartCounterUpdate.GetAction() {
		case pb.RestartCounterUpdate_ACTION_INCREMENT:
			atomic.AddInt64(&r.counterInc, 1)
		case pb.RestartCounterUpdate_ACTION_RESET:
			atomic.AddInt64(&r.counterRst, 1)
		}
	}
}

func newHarness(t *testing.T, leader bool) (*health.Restarter, *state.State, *recordingApplier) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	rec := newRecordingApplier(f, &raftIdx)
	r := health.New(st, brokers, &fakeLeader{leader: leader}, rec.Apply)
	return r, st, rec
}

func failedEvent(id string) watch.Event[*pb.ReplicaObserved] {
	return watch.Event[*pb.ReplicaObserved]{
		Kind: watch.KindUpdated,
		After: &pb.ReplicaObserved{
			Id: id, State: pb.ReplicaState_REPLICA_STATE_FAILED,
			LastHealthAt: timestamppb.Now(),
		},
	}
}

func TestHandle_FirstFailureIncrementsCounterAndIssuesRestart(t *testing.T) {
	r, st, rec := newHarness(t, true)
	r.Handle(failedEvent("sample-web-0"))

	rc, ok := st.RestartCounters.Get("sample-web-0")
	if !ok {
		t.Fatalf("RestartCounter missing post-first-failure")
	}
	if rc.GetConsecutiveFailures() != 1 {
		t.Errorf("consecutive_failures = %d, want 1", rc.GetConsecutiveFailures())
	}
	if got := atomic.LoadInt64(&rec.restartIss); got != 1 {
		t.Errorf("restart commands emitted = %d, want 1", got)
	}
}

func TestHandle_NoRestartAfterThreeConsecutiveFailures(t *testing.T) {
	r, st, rec := newHarness(t, true)

	// Three failures in a row.
	r.Handle(failedEvent("sample-web-0"))
	r.Handle(failedEvent("sample-web-0"))
	r.Handle(failedEvent("sample-web-0"))

	if got := atomic.LoadInt64(&rec.restartIss); got != 2 {
		// First two failures issued restarts; the third bypassed restart
		// and went straight to restart_exhausted (no fourth-restart issued).
		t.Errorf("restart commands = %d, want 2 (third failure goes to exhausted)", got)
	}
	if got := atomic.LoadInt64(&rec.exhausted); got != 1 {
		t.Errorf("restart_exhausted writes = %d, want 1", got)
	}
	obs, ok := st.ReplicasObserved.Get("sample-web-0")
	if !ok {
		t.Fatalf("ReplicaObserved missing")
	}
	if obs.GetState() != pb.ReplicaState_REPLICA_STATE_FAILED || obs.GetCode() != "restart_exhausted" {
		t.Errorf("final state = %v / code = %q; want FAILED / restart_exhausted",
			obs.GetState(), obs.GetCode())
	}

	// Fourth failure — must NOT emit another restart (the AC).
	prevRestart := atomic.LoadInt64(&rec.restartIss)
	r.Handle(failedEvent("sample-web-0"))
	if got := atomic.LoadInt64(&rec.restartIss); got != prevRestart {
		t.Errorf("restart emitted on fourth failure: %d (want %d)", got, prevRestart)
	}
}

func TestHandle_RunningResetsCounter(t *testing.T) {
	r, st, rec := newHarness(t, true)
	r.Handle(failedEvent("sample-web-0")) // counter -> 1
	r.Handle(failedEvent("sample-web-0")) // counter -> 2
	if rc, ok := st.RestartCounters.Get("sample-web-0"); !ok || rc.GetConsecutiveFailures() != 2 {
		t.Fatalf("preconditions: counter = %d/%v", rc.GetConsecutiveFailures(), ok)
	}
	// Running observation should reset.
	r.Handle(watch.Event[*pb.ReplicaObserved]{
		Kind:  watch.KindUpdated,
		After: &pb.ReplicaObserved{Id: "sample-web-0", State: pb.ReplicaState_REPLICA_STATE_RUNNING, LastHealthAt: timestamppb.Now()},
	})
	if _, ok := st.RestartCounters.Get("sample-web-0"); ok {
		t.Errorf("counter should be reset after RUNNING; still present")
	}
	if got := atomic.LoadInt64(&rec.counterRst); got != 1 {
		t.Errorf("counter resets = %d, want 1", got)
	}

	// Subsequent failure starts the count over at 1.
	r.Handle(failedEvent("sample-web-0"))
	if rc, _ := st.RestartCounters.Get("sample-web-0"); rc.GetConsecutiveFailures() != 1 {
		t.Errorf("counter after reset+failure = %d, want 1", rc.GetConsecutiveFailures())
	}
}

func TestHandle_DegradedEmitsRemoveFromRoutingPlusRestart(t *testing.T) {
	r, _, rec := newHarness(t, true)
	r.Handle(watch.Event[*pb.ReplicaObserved]{
		Kind:  watch.KindUpdated,
		After: &pb.ReplicaObserved{Id: "sample-web-0", State: pb.ReplicaState_REPLICA_STATE_DEGRADED},
	})
	if got := atomic.LoadInt64(&rec.removeRout); got != 1 {
		t.Errorf("remove_from_routing = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&rec.restartIss); got != 1 {
		t.Errorf("restart commands = %d, want 1", got)
	}
}

func TestHandle_NoOpOnFollower(t *testing.T) {
	r, _, rec := newHarness(t, false /* leader=false */)
	r.Handle(failedEvent("sample-web-0"))
	if got := atomic.LoadInt64(&rec.restartIss); got != 0 {
		t.Errorf("restart emitted on follower: %d", got)
	}
}

func TestHandle_IgnoresOwnRestartExhaustedReply(t *testing.T) {
	// The restarter's own write of restart_exhausted appears as a
	// ReplicasObserved Updated event. Processing it would create an
	// infinite loop. Verify we filter it out.
	r, _, rec := newHarness(t, true)
	r.Handle(watch.Event[*pb.ReplicaObserved]{
		Kind: watch.KindUpdated,
		After: &pb.ReplicaObserved{
			Id: "sample-web-0", State: pb.ReplicaState_REPLICA_STATE_FAILED,
			Code: "restart_exhausted",
		},
	})
	if got := atomic.LoadInt64(&rec.counterInc); got != 0 {
		t.Errorf("counter incremented on restart_exhausted echo: %d", got)
	}
}
