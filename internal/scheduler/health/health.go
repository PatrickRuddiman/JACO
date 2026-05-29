// Package health is the scheduler's restart policy. Distinct from
// internal/runtime/health which polls the docker engine — this package
// reacts to ReplicaObserved state transitions (written by the runtime) by
// emitting ReplicaCommand{op:restart} / {op:remove_from_routing} commands
// and the closed 3-strike RestartCounter policy.
//
// Subscribes to the ReplicasObserved broker on the raft leader; followers
// observe but emit nothing (the loop self-gates on LeaderStatus).
package health

import (
	"context"
	"log/slog"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// MaxConsecutiveFailures is the 3-strike threshold from the scheduler slice
// §4. After this many failures with no intervening RUNNING, the replica is
// marked failed/restart_exhausted and no more restarts fire until the next
// Deploy.Apply (which clears the RestartCounter via the runtime's first
// healthy poll).
const MaxConsecutiveFailures = 3

// Applier wraps raft.Apply.
type Applier func(cmd []byte) error

// Restarter consumes the ReplicasObserved broker and applies the restart
// policy to FAILED / DEGRADED / RUNNING transitions.
type Restarter struct {
	state   *state.State
	brokers *watch.Registry
	leader  scheduler.LeaderStatus
	apply   Applier

	// Logger logs restart decisions. nil → discard. Set by the daemon after
	// construction; tests leave it nil.
	Logger *slog.Logger

	readyOnce sync.Once
	ready     chan struct{}

	handledMu sync.Mutex
	handled   chan watch.Event[*pb.ReplicaObserved]
}

func (r *Restarter) log() *slog.Logger {
	if r.Logger == nil {
		return logging.Discard()
	}
	return r.Logger
}

// New constructs a Restarter.
func New(s *state.State, brokers *watch.Registry, leader scheduler.LeaderStatus, apply Applier) *Restarter {
	return &Restarter{
		state:   s,
		brokers: brokers,
		leader:  leader,
		apply:   apply,
		ready:   make(chan struct{}),
	}
}

// Ready returns a channel closed once Run has registered its broker
// subscription. Callers that publish events immediately after starting Run
// (notably tests driving the FSM directly) wait on this to avoid racing the
// subscribe call and dropping events on the floor.
func (r *Restarter) Ready() <-chan struct{} { return r.ready }

// NotifyHandled installs a channel that receives every event after Handle
// returns. Used by tests to sync on event consumption without polling state.
// Passing nil clears the hook. Sends are non-blocking; size the channel for
// the expected event volume.
func (r *Restarter) NotifyHandled(ch chan watch.Event[*pb.ReplicaObserved]) {
	r.handledMu.Lock()
	r.handled = ch
	r.handledMu.Unlock()
}

// Run subscribes to ReplicasObserved and dispatches each event to the
// restart policy. Blocks until ctx is cancelled.
func (r *Restarter) Run(ctx context.Context) error {
	sub := r.brokers.ReplicasObserved.Subscribe()
	defer sub.Cancel()
	r.readyOnce.Do(func() { close(r.ready) })
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			r.Handle(ev)
			r.notifyHandled(ev)
		}
	}
}

func (r *Restarter) notifyHandled(ev watch.Event[*pb.ReplicaObserved]) {
	r.handledMu.Lock()
	ch := r.handled
	r.handledMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}

// Handle applies the restart policy to a single ReplicaObserved event. No-op
// when ev.After is nil or the local node isn't the raft leader.
func (r *Restarter) Handle(ev watch.Event[*pb.ReplicaObserved]) {
	if ev.After == nil || !r.leader.IsLeader() {
		return
	}
	obs := ev.After
	switch obs.GetState() {
	case pb.ReplicaState_REPLICA_STATE_FAILED:
		// Skip terminal restart_exhausted observations — those are
		// self-emitted writes; processing them would loop.
		if obs.GetCode() == "restart_exhausted" {
			return
		}
		r.handleFailure(obs)
	case pb.ReplicaState_REPLICA_STATE_DEGRADED:
		r.handleDegraded(obs)
	case pb.ReplicaState_REPLICA_STATE_RUNNING:
		r.handleRunning(obs)
	}
}

func (r *Restarter) handleFailure(obs *pb.ReplicaObserved) {
	// Read the current counter; the about-to-apply value is current+1.
	var nextFailures int32 = 1
	if existing, ok := r.state.RestartCounters.Get(obs.GetId()); ok {
		nextFailures = existing.GetConsecutiveFailures() + 1
	}
	if nextFailures >= MaxConsecutiveFailures {
		r.log().Warn("restart policy exhausted, marking replica failed",
			logging.KeyReplicaID, obs.GetId(), "consecutive_failures", nextFailures)
		// Mark restart_exhausted and stop. The FSM accepts this update via
		// Command{ReplicaObservedUpdate}.
		exhausted := &pb.ReplicaObserved{
			Id:           obs.GetId(),
			State:        pb.ReplicaState_REPLICA_STATE_FAILED,
			Code:         "restart_exhausted",
			Message:      "restart policy gave up after 3 consecutive failures",
			ContainerId:  obs.GetContainerId(),
			Host:         obs.GetHost(),
			LastHealthAt: obs.GetLastHealthAt(),
			Details:      map[string]string{"consecutive_failures": strconv.Itoa(int(nextFailures))},
		}
		r.applyOne(&pb.Command{
			Identity: "restarter",
			Ts:       timestamppb.Now(),
			Payload:  &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: exhausted}},
		})
		return
	}
	// Bump counter + emit restart command (batched).
	r.log().Info("restarting failed replica",
		logging.KeyReplicaID, obs.GetId(), logging.KeyReason, "failed",
		"consecutive_failures", nextFailures, "code", obs.GetCode())
	batch := &pb.Batch{Children: []*pb.Command{
		{
			Identity: "restarter",
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_RestartCounterUpdate{RestartCounterUpdate: &pb.RestartCounterUpdate{
				ReplicaId: obs.GetId(),
				Action:    pb.RestartCounterUpdate_ACTION_INCREMENT,
			}},
		},
		{
			Identity: "restarter",
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_ReplicaCommandIssue{ReplicaCommandIssue: &pb.ReplicaCommandIssue{
				ReplicaId: obs.GetId(),
				Op:        "restart",
			}},
		},
	}}
	r.applyOne(&pb.Command{
		Identity: "restarter",
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_Batch{Batch: batch},
	})
}

func (r *Restarter) handleDegraded(obs *pb.ReplicaObserved) {
	r.log().Info("removing degraded replica from routing and restarting",
		logging.KeyReplicaID, obs.GetId(), logging.KeyReason, "degraded")
	// Pull from ingress rotation + restart.
	batch := &pb.Batch{Children: []*pb.Command{
		{
			Identity: "restarter",
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_ReplicaCommandIssue{ReplicaCommandIssue: &pb.ReplicaCommandIssue{
				ReplicaId: obs.GetId(),
				Op:        "remove_from_routing",
			}},
		},
		{
			Identity: "restarter",
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_ReplicaCommandIssue{ReplicaCommandIssue: &pb.ReplicaCommandIssue{
				ReplicaId: obs.GetId(),
				Op:        "restart",
			}},
		},
	}}
	r.applyOne(&pb.Command{
		Identity: "restarter",
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_Batch{Batch: batch},
	})
}

func (r *Restarter) handleRunning(obs *pb.ReplicaObserved) {
	// Reset the counter if one exists; idempotent otherwise.
	if _, ok := r.state.RestartCounters.Get(obs.GetId()); !ok {
		return
	}
	r.applyOne(&pb.Command{
		Identity: "restarter",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_RestartCounterUpdate{RestartCounterUpdate: &pb.RestartCounterUpdate{
			ReplicaId: obs.GetId(),
			Action:    pb.RestartCounterUpdate_ACTION_RESET,
		}},
	})
}

func (r *Restarter) applyOne(cmd *pb.Command) {
	data, err := proto.Marshal(cmd)
	if err != nil {
		r.log().Error("restarter marshal command failed", "error", err)
		return
	}
	if err := r.apply(data); err != nil {
		r.log().Error("restarter raft apply failed", "error", err)
	}
}
