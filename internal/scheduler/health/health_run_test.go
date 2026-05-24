package health_test

import (
	"context"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/health"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestRun_FiresOnFailureEvent — Run subscribes to ReplicasObserved and
// dispatches each event through Handle. We apply a FAILED observation
// while Run is active and confirm the restart counter increments.
func TestRun_FiresOnFailureEvent(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	r := health.New(st, brokers, &fakeLeader{leader: true}, apply)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait for Run's goroutine to register its subscription before we
	// publish — otherwise the FSM Apply below races the Subscribe call
	// in Run and the event is dropped on the floor.
	subDeadline := time.Now().Add(2 * time.Second)
	for brokers.ReplicasObserved.SubscriberCount() == 0 {
		if time.Now().After(subDeadline) {
			t.Fatalf("Run did not subscribe to ReplicasObserved within 2s")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Apply a FAILED ReplicaObserved through the FSM (will fire the
	// watch broker).
	raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaObservedUpdate{
		ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: &pb.ReplicaObserved{
			Id: "smoke-web-0", State: pb.ReplicaState_REPLICA_STATE_FAILED,
		}},
	}}
	data, _ := proto.Marshal(cmd)
	f.Apply(&hraft.Log{Index: raftIdx, Data: data})

	// Wait briefly for Restarter.Run to consume the event and apply
	// the batch (increment + restart command).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, ok := st.RestartCounters.Get("smoke-web-0"); ok && c.GetConsecutiveFailures() == 1 {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Run did not produce a restart counter increment for the FAILED replica")
}

// TestRun_ContextCancelExits — Run returns ctx.Err() on cancel.
func TestRun_ContextCancelExits(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	r := health.New(st, brokers, &fakeLeader{leader: false}, func([]byte) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Run did not return after cancel")
	}
}

// TestHandle_NilAfterIsNoop — Handle defends against events with no
// After payload (KindRemoved is the typical source). Doesn't apply
// anything.
func TestHandle_NilAfterIsNoop(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	r := health.New(st, brokers, &fakeLeader{leader: true}, func([]byte) error {
		t.Errorf("apply called on nil-After event")
		return nil
	})
	r.Handle(watch.Event[*pb.ReplicaObserved]{Kind: watch.KindRemoved, Before: &pb.ReplicaObserved{Id: "x"}, After: nil})
}

// TestHandle_FollowerSkips — when the local node isn't the raft
// leader, Handle is a no-op (the leader emits restarts so followers
// don't double-apply).
func TestHandle_FollowerSkips(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	r := health.New(st, brokers, &fakeLeader{leader: false}, func([]byte) error {
		t.Errorf("apply called on follower")
		return nil
	})
	r.Handle(watch.Event[*pb.ReplicaObserved]{Kind: watch.KindUpdated, After: &pb.ReplicaObserved{
		Id: "x", State: pb.ReplicaState_REPLICA_STATE_FAILED,
	}})
}
