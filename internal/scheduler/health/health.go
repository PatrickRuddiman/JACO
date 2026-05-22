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
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// MaxConsecutiveFailures is the 3-strike threshold from the scheduler slice
// §4. After this many failures with no intervening RUNNING, the replica is
// marked failed/restart_exhausted and no more restarts fire until the next
// Deploy.Apply (which clears the RestartCounter via the runtime's first
// healthy poll).
const MaxConsecutiveFailures = 3

// LeaderStatus + Applier types match the other scheduler subpackages.
type LeaderStatus interface {
	IsLeader() bool
}

type Applier func(cmd []byte) error

// Restarter consumes the ReplicasObserved broker and applies the restart
// policy to FAILED / DEGRADED / RUNNING transitions.
type Restarter struct {
	state   *state.State
	brokers *watch.Registry
	leader  LeaderStatus
	apply   Applier
}

// New constructs a Restarter.
func New(s *state.State, brokers *watch.Registry, leader LeaderStatus, apply Applier) *Restarter {
	return &Restarter{state: s, brokers: brokers, leader: leader, apply: apply}
}

// Run subscribes to ReplicasObserved and dispatches each event to the
// restart policy. Blocks until ctx is cancelled.
func (r *Restarter) Run(ctx context.Context) error {
	sub := r.brokers.ReplicasObserved.Subscribe()
	defer sub.Cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			r.Handle(ev)
		}
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
		return
	}
	_ = r.apply(data)
}
