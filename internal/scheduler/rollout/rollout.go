// Package rollout owns the per-service rolling-update state machine.
// Drives the RolloutPlan entity through (IN_PROGRESS → COMPLETED) or
// (IN_PROGRESS → ABORTED), enforces the never-below-replicas-1 invariant,
// and aborts via DeploymentRollback when a step exceeds StepTimeout.
//
// The scheduler decides WHEN to start / advance / complete / abort the plan;
// this package supplies the bookkeeping + raft-Apply orchestration.
package rollout

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/counter"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Constants from the scheduler slice §4.
const (
	StepTimeout     = 60 * time.Second
	HealthFreshness = 10 * time.Second
)

// Applier wraps raft.Apply.
type Applier func(cmd []byte) error

// Rollout drives RolloutPlan entities.
type Rollout struct {
	state *state.State
	apply Applier
	now   func() time.Time

	// Logger logs step transitions. nil → discard. Set by the daemon after
	// construction; tests leave it nil.
	Logger *slog.Logger
}

func (r *Rollout) log() *slog.Logger {
	if r.Logger == nil {
		return logging.Discard()
	}
	return r.Logger
}

// New constructs a Rollout. now=nil falls through to time.Now; tests pass
// a fake to advance time without sleeps.
func New(s *state.State, apply Applier, now func() time.Time) *Rollout {
	if now == nil {
		now = time.Now
	}
	return &Rollout{state: s, apply: apply, now: now}
}

// Start creates a new RolloutPlan with state IN_PROGRESS. Refuses when a
// plan is already in progress for (deployment, service).
func (r *Rollout) Start(deployment, service string, targetRev uint64, totalSteps int) error {
	if existing, ok := r.state.RolloutPlans.Get(state.RolloutPlanKey(deployment, service)); ok {
		if existing.GetState() == pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
			return fmt.Errorf("rollout already in progress for %s/%s (current_step=%d, total_steps=%d)",
				deployment, service, existing.GetCurrentStep(), existing.GetTotalSteps())
		}
	}
	now := r.now()
	plan := &pb.RolloutPlan{
		Deployment:     deployment,
		Service:        service,
		TargetRevision: targetRev,
		TotalSteps:     int32(totalSteps),
		CurrentStep:    0,
		State:          pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS,
		StartedAt:      timestamppb.New(now),
		LastStepAt:     timestamppb.New(now),
	}
	r.log().Info("rollout started",
		logging.KeyDeployment, deployment, "service", service,
		"target_revision", targetRev, "total_steps", totalSteps)
	return r.applyPlan(plan)
}

// StepReady reports whether the rollout can advance from its current step.
// `ready=true` means the current-step replica reports state=RUNNING with
// fresh health (last_health_at within HealthFreshness). Returns notRunning
// count for the invariant check.
func (r *Rollout) StepReady(deployment, service string) (ready bool, notRunning int, err error) {
	plan, ok := r.state.RolloutPlans.Get(state.RolloutPlanKey(deployment, service))
	if !ok {
		return false, 0, fmt.Errorf("no rollout plan for %s/%s", deployment, service)
	}
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		return false, 0, nil
	}

	for _, obs := range r.state.ReplicasObserved.List() {
		if obs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
			// Only count replicas that belong to this service.
			if r.belongsToService(obs, deployment, service) {
				notRunning++
			}
		}
	}

	targetID := counter.ReplicaID(deployment, service, uint64(plan.GetCurrentStep()))
	targetObs, ok := r.state.ReplicasObserved.Get(targetID)
	if !ok {
		return false, notRunning, nil
	}
	if targetObs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
		return false, notRunning, nil
	}
	if !r.isFresh(targetObs.GetLastHealthAt()) {
		return false, notRunning, nil
	}
	return true, notRunning, nil
}

// AdvanceStep bumps current_step and refreshes last_step_at. Refuses to
// advance when the never-below-replicas-1 invariant would be violated
// (more than 1 replica currently not-running in this service).
func (r *Rollout) AdvanceStep(deployment, service string) error {
	plan, ok := r.state.RolloutPlans.Get(state.RolloutPlanKey(deployment, service))
	if !ok {
		return fmt.Errorf("no rollout plan for %s/%s", deployment, service)
	}
	if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		return fmt.Errorf("rollout %s/%s state = %v; cannot advance", deployment, service, plan.GetState())
	}
	// Invariant: at most 1 replica not-running.
	_, notRunning, err := r.StepReady(deployment, service)
	if err != nil {
		return err
	}
	if notRunning > 1 {
		return r.auditAndHold(deployment, service,
			fmt.Sprintf("invariant_hold: %d replicas not running (>1)", notRunning))
	}

	plan.CurrentStep++
	plan.LastStepAt = timestamppb.New(r.now())
	r.log().Info("rollout step advanced",
		logging.KeyDeployment, deployment, "service", service,
		"current_step", plan.GetCurrentStep(), "total_steps", plan.GetTotalSteps())
	return r.applyPlan(plan)
}

// Complete marks the rollout completed.
func (r *Rollout) Complete(deployment, service string) error {
	plan, ok := r.state.RolloutPlans.Get(state.RolloutPlanKey(deployment, service))
	if !ok {
		return fmt.Errorf("no rollout plan for %s/%s", deployment, service)
	}
	plan.State = pb.RolloutState_ROLLOUT_STATE_COMPLETED
	plan.LastStepAt = timestamppb.New(r.now())
	r.log().Info("rollout completed",
		logging.KeyDeployment, deployment, "service", service,
		"total_steps", plan.GetTotalSteps())
	return r.applyPlan(plan)
}

// Abort marks the rollout aborted, audits ROLLBACK, and raft-Applies a
// DeploymentRollback to restore the previous Deployment revision. The two
// state changes land as one Command{Batch}.
func (r *Rollout) Abort(_ context.Context, deployment, service, reason string) error {
	plan, ok := r.state.RolloutPlans.Get(state.RolloutPlanKey(deployment, service))
	if !ok {
		return fmt.Errorf("no rollout plan for %s/%s", deployment, service)
	}
	plan.State = pb.RolloutState_ROLLOUT_STATE_ABORTED
	plan.FailureReason = reason
	plan.LastStepAt = timestamppb.New(r.now())
	r.log().Warn("rollout aborted, rolling back deployment",
		logging.KeyDeployment, deployment, "service", service, logging.KeyReason, reason)

	planCmd := planCommand(plan, r.now())

	// Build the deployment rollback command. The FSM's DeploymentRollback
	// handler will flip applied_revision <-> previous_revision and audit
	// ROLLBACK.
	var children []*pb.Command
	children = append(children, planCmd)
	if dep, ok := r.state.Deployments.Get(deployment); ok && dep.GetPreviousRevision() > 0 {
		children = append(children, &pb.Command{
			Identity: "rollout",
			Ts:       timestamppb.New(r.now()),
			Payload: &pb.Command_DeploymentRollback{DeploymentRollback: &pb.DeploymentRollback{
				Deployment: deployment,
				Revision:   dep.GetPreviousRevision(),
			}},
		})
	}

	batch := &pb.Command{
		Identity: "rollout",
		Ts:       timestamppb.New(r.now()),
		Payload:  &pb.Command_Batch{Batch: &pb.Batch{Children: children}},
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		return err
	}
	return r.apply(data)
}

// CheckTimeouts scans every IN_PROGRESS RolloutPlan and aborts those that
// have exceeded StepTimeout. Called from the scheduler's 30s safety tick.
// Returns the deployment names of every aborted plan.
func (r *Rollout) CheckTimeouts(ctx context.Context) ([]string, error) {
	var aborted []string
	now := r.now()
	for _, plan := range r.state.RolloutPlans.List() {
		if plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
			continue
		}
		if plan.GetLastStepAt() == nil {
			continue
		}
		if now.Sub(plan.GetLastStepAt().AsTime()) < StepTimeout {
			continue
		}
		if err := r.Abort(ctx, plan.GetDeployment(), plan.GetService(), "step_timeout"); err != nil {
			return aborted, err
		}
		aborted = append(aborted, plan.GetDeployment())
	}
	return aborted, nil
}

func (r *Rollout) applyPlan(plan *pb.RolloutPlan) error {
	cmd := planCommand(plan, r.now())
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return r.apply(data)
}

func planCommand(plan *pb.RolloutPlan, now time.Time) *pb.Command {
	return &pb.Command{
		Identity: "rollout",
		Ts:       timestamppb.New(now),
		Payload:  &pb.Command_RolloutPlanUpdate{RolloutPlanUpdate: &pb.RolloutPlanUpdate{Plan: plan}},
	}
}

// auditAndHold emits an AuditAppend{type:ROLLOUT_INVARIANT_HOLD} but does
// NOT mutate the plan. The scheduler retries on the next tick.
func (r *Rollout) auditAndHold(deployment, service, reason string) error {
	cmd := &pb.Command{
		Identity: "rollout",
		Ts:       timestamppb.New(r.now()),
		Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{
				Type:     pb.AuditEventType_AUDIT_EVENT_TYPE_ROLLOUT_INVARIANT_HOLD,
				Identity: "rollout",
				Payload: map[string]string{
					"deployment": deployment,
					"service":    service,
					"reason":     reason,
				},
			},
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return r.apply(data)
}

// belongsToService reports whether a ReplicaObserved belongs to the given
// (deployment, service). ReplicaObserved carries only an Id of the form
// <deployment>-<service>-<index> (see counter.ReplicaID), which is ambiguous
// when names contain dashes. The authoritative mapping lives in
// state.ReplicasDesired, whose entries carry explicit Deployment/Service
// fields keyed by the same Id, so we look the observed replica up there and
// compare. A replica with no desired entry is an orphan and belongs to no
// service (returns false), so it never inflates another service's
// not-running count.
func (r *Rollout) belongsToService(obs *pb.ReplicaObserved, deployment, service string) bool {
	desired, ok := r.state.ReplicasDesired.Get(obs.GetId())
	if !ok {
		return false
	}
	return desired.GetDeployment() == deployment && desired.GetService() == service
}

// isFresh reports whether the timestamp is within HealthFreshness of now.
// A nil timestamp is never fresh.
func (r *Rollout) isFresh(ts *timestamppb.Timestamp) bool {
	if ts == nil {
		return false
	}
	return r.now().Sub(ts.AsTime()) < HealthFreshness
}
