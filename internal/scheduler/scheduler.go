// Package scheduler is the leader-only desired-state reconciler. Subscribes
// to Deployments / Nodes / ReplicasObserved watches; on every event (50ms
// debounced) or every 30s safety tick, computes the desired ReplicaDesired
// set for every (deployment, service), diffs against current state, and
// raft-Applies the resulting adds / updates / removes as a single
// Command{Batch}.
//
// Run only on the raft leader. When the local node loses leadership, all
// watch subscriptions get cancelled and reconcile() becomes a no-op until
// leadership returns.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/counter"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/placement"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rollout"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// LeaderStatus is the interface Run uses to gate reconciliation on
// leadership. raftnode.Node satisfies this directly; tests pass a
// controllable fake.
type LeaderStatus interface {
	IsLeader() bool
}

// Applier wraps raft.Apply.
type Applier func(cmd []byte) error

// Cadence constants from the slice §3.
const (
	DebounceWindow = 50 * time.Millisecond
	SafetyTick     = 30 * time.Second
)

// Scheduler holds the dependencies for one daemon's reconciler.
type Scheduler struct {
	state   *state.State
	brokers *watch.Registry
	leader  LeaderStatus
	apply   Applier

	// rollouts drives image-change orchestration. nil → fall back to the
	// minimal one-replica-per-pass image swap from iter 29 (still safe;
	// just no formal plan / audit / rollback-on-failure).
	rollouts *rollout.Rollout

	// Logger logs reconcile milestones. nil → discard. Set by the daemon
	// after construction; tests leave it nil.
	Logger *slog.Logger

	mu     sync.Mutex
	active bool
	cancel context.CancelFunc
}

func (s *Scheduler) log() *slog.Logger {
	if s.Logger == nil {
		return logging.Discard()
	}
	return s.Logger
}

// New constructs a Scheduler. rollouts may be nil for callers that don't
// need the formal rollout state machine (existing tests).
func New(s *state.State, brokers *watch.Registry, leader LeaderStatus, apply Applier, rollouts *rollout.Rollout) *Scheduler {
	return &Scheduler{state: s, brokers: brokers, leader: leader, apply: apply, rollouts: rollouts}
}

// Run drives the reconcile loop. Blocks until ctx is cancelled. Should be
// invoked in a goroutine from the daemon entry; the caller polls
// `leader.IsLeader()` and calls Start/Stop accordingly, or — in v1 — the
// caller invokes Run unconditionally and the reconcile loop self-gates via
// leader.IsLeader().
func (s *Scheduler) Run(ctx context.Context) error {
	deps := s.brokers.Deployments.Subscribe()
	defer deps.Cancel()
	nodes := s.brokers.Nodes.Subscribe()
	defer nodes.Cancel()
	obs := s.brokers.ReplicasObserved.Subscribe()
	defer obs.Cancel()

	// Initial reconcile so the daemon catches up on boot.
	s.Reconcile(ctx)

	ticker := time.NewTicker(SafetyTick)
	defer ticker.Stop()

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	pending := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deps.Events():
			pending = true
			debounce.Reset(DebounceWindow)
		case <-nodes.Events():
			pending = true
			debounce.Reset(DebounceWindow)
		case <-obs.Events():
			pending = true
			debounce.Reset(DebounceWindow)
		case <-debounce.C:
			if pending {
				pending = false
				s.Reconcile(ctx)
			}
		case <-ticker.C:
			s.Reconcile(ctx)
		}
	}
}

// Reconcile runs one reconcile pass. No-op when the local node isn't the
// raft leader. Exposed publicly so unit tests can drive it without spinning
// up the long-running Run loop.
func (s *Scheduler) Reconcile(_ context.Context) {
	if !s.leader.IsLeader() {
		return
	}

	// Abort any rollouts that timed out before placing new replicas.
	// When CheckTimeouts aborts a plan, also skip the per-deployment
	// reconcile this tick so the abort's DeploymentRollback lands cleanly
	// without immediately re-starting a "roll the upgrade back" rollout
	// on the same pass — that fires naturally on the next tick from the
	// post-rollback state.
	abortedThisTick := map[string]bool{}
	if s.rollouts != nil {
		aborted, _ := s.rollouts.CheckTimeouts(context.Background())
		for _, name := range aborted {
			abortedThisTick[name] = true
		}
	}

	deployments := s.state.Deployments.List()
	nodes := s.state.Nodes.List()

	var batch []*pb.Command

	for _, dep := range deployments {
		if abortedThisTick[dep.GetName()] {
			continue
		}
		project, err := compose.LoadBytes(dep.GetComposeYaml(), "deploy-compose.yml")
		if err != nil {
			// Mark Deployment pending so the operator can see the failure
			// in `jaco status`.
			batch = append(batch, deploymentStatusPending(dep.GetName(),
				fmt.Sprintf("compose parse failed: %v", err)))
			continue
		}
		for _, svc := range dep.GetServices() {
			cmds := s.reconcileService(dep, svc, nodes, project)
			batch = append(batch, cmds...)
		}
	}

	if len(batch) == 0 {
		return
	}

	combined := &pb.Command{
		Identity: "scheduler",
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_Batch{Batch: &pb.Batch{Children: batch}},
	}
	data, err := proto.Marshal(combined)
	if err != nil {
		s.log().Error("scheduler marshal reconcile batch failed", "error", err)
		return
	}
	s.log().Info("applying reconcile batch", "commands", len(batch))
	if err := s.apply(data); err != nil {
		s.log().Error("scheduler reconcile batch apply failed", "commands", len(batch), "error", err)
	}
}

// reconcileService computes the diff between current and desired
// ReplicaDesired for one service. Returns the Command list (may be empty
// when current already matches desired).
func (s *Scheduler) reconcileService(dep *pb.Deployment, svc *pb.ServiceSpec, nodes []*pb.Node, project *composeProject) []*pb.Command {
	image := lookupImage(project, svc.GetName())
	if image == "" {
		return []*pb.Command{deploymentStatusPending(dep.GetName(),
			fmt.Sprintf("service %q not found in compose project", svc.GetName()))}
	}

	eligible := placement.EligibleHosts(svc, nodes)

	// Per-host current replica counts feed PACK placement decisions.
	currentCounts := map[string]int{}
	currentByID := map[string]*pb.ReplicaDesired{}
	for _, r := range s.state.ReplicasDesired.List() {
		if r.GetDeployment() == dep.GetName() && r.GetService() == svc.GetName() {
			currentByID[r.GetId()] = r
			currentCounts[r.GetHost()]++
		}
	}

	var cmds []*pb.Command

	// 1. Build the desired set.
	type desiredReplica struct {
		id    string
		host  string
		index int32
	}
	var desired []desiredReplica
	for i := int32(0); i < svc.GetReplicas(); i++ {
		host, err := placement.PlaceReplica(dep.GetName(), svc, eligible, int(i), currentCounts)
		if err != nil {
			// Pinned-host placement failure → DeploymentStatusUpdate
			// pending, place no replicas for this service this pass.
			return []*pb.Command{deploymentStatusPending(dep.GetName(), err.Error())}
		}
		desired = append(desired, desiredReplica{
			id:    counter.ReplicaID(dep.GetName(), svc.GetName(), uint64(i)),
			host:  host,
			index: i,
		})
	}

	// Image-change detection. If the formal rollout state machine is
	// wired (s.rollouts != nil), drive it: start a plan on first
	// detection, only emit upsert for the CurrentStep replica per pass,
	// AdvanceStep when StepReady, Complete when CurrentStep ==
	// TotalSteps. When s.rollouts is nil (test paths that don't need
	// the formal machine) fall back to the minimal one-at-a-time gate
	// from iter 29.
	rolling := isRollingImageChange(currentByID, image)
	imageChangedThisPass := false
	rolloutStep := int32(-1) // -1 = no plan driving this pass
	if s.rollouts != nil && rolling {
		rolloutStep = s.driveRollout(dep, svc, int32(svc.GetReplicas()))
	}

	// 2. Adds + updates.
	desiredIDs := map[string]bool{}
	for _, d := range desired {
		desiredIDs[d.id] = true
		rep := &pb.ReplicaDesired{
			Id:         d.id,
			Deployment: dep.GetName(),
			Service:    svc.GetName(),
			Index:      d.index,
			Host:       d.host,
			Image:      image,
		}
		if cur, ok := currentByID[d.id]; ok {
			if cur.GetHost() == d.host && cur.GetImage() == image {
				continue // already matches desired
			}
			// Image-only change while rolling — gate by either the
			// rollout-driven CurrentStep (when rollouts != nil) or the
			// iter-29 one-at-a-time fallback (when nil).
			if rolling && cur.GetHost() == d.host && cur.GetImage() != image {
				if rolloutStep >= 0 {
					if d.index != rolloutStep {
						continue
					}
				} else {
					if imageChangedThisPass {
						continue
					}
					imageChangedThisPass = true
				}
			}
		}
		cmds = append(cmds, &pb.Command{
			Identity: "scheduler",
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_ReplicaDesiredUpsert{ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
				Replica: rep,
			}},
		})
	}

	// 3. Removes — any replica currently desired but not in the target set.
	for id := range currentByID {
		if desiredIDs[id] {
			continue
		}
		cmds = append(cmds, &pb.Command{
			Identity: "scheduler",
			Ts:       timestamppb.Now(),
			Payload:  &pb.Command_ReplicaDesiredRemove{ReplicaDesiredRemove: &pb.ReplicaDesiredRemove{Id: id}},
		})
	}

	return cmds
}

// driveRollout returns the replica index the current reconcile pass
// should upsert. -1 means "no rollout in progress / no advance this
// pass" (callers fall back to nothing-to-do for image changes).
//
// On first detection of an image change, Start a plan for replicas 0…N-1.
// When a plan is IN_PROGRESS and the current-step replica is RUNNING with
// fresh health, AdvanceStep. When CurrentStep == TotalSteps, Complete.
func (s *Scheduler) driveRollout(dep *pb.Deployment, svc *pb.ServiceSpec, totalSteps int32) int32 {
	key := state.RolloutPlanKey(dep.GetName(), svc.GetName())
	plan, ok := s.state.RolloutPlans.Get(key)
	if !ok || plan.GetState() != pb.RolloutState_ROLLOUT_STATE_IN_PROGRESS {
		// Refuse to restart a rollout whose plan already exists for this
		// revision — ABORTED / COMPLETED are terminal states. Otherwise
		// CheckTimeouts → Abort would just re-fire Start on the next
		// reconcile in an infinite loop.
		if ok && plan.GetTargetRevision() == dep.GetAppliedRevision() {
			return -1
		}
		if err := s.rollouts.Start(dep.GetName(), svc.GetName(), dep.GetAppliedRevision(), int(totalSteps)); err != nil {
			// Start refuses when another plan is already IN_PROGRESS;
			// fall back to "wait" (no upsert this pass).
			return -1
		}
		// First step is index 0.
		return 0
	}
	cur := plan.GetCurrentStep()
	if cur >= plan.GetTotalSteps() {
		_ = s.rollouts.Complete(dep.GetName(), svc.GetName())
		return -1
	}
	ready, _, err := s.rollouts.StepReady(dep.GetName(), svc.GetName())
	if err != nil {
		return -1
	}
	if ready {
		if err := s.rollouts.AdvanceStep(dep.GetName(), svc.GetName()); err != nil {
			return -1
		}
		// AdvanceStep bumped current_step; the new step is what we
		// upsert this pass. Re-read for the latest value.
		plan, ok = s.state.RolloutPlans.Get(key)
		if !ok {
			return -1
		}
		cur = plan.GetCurrentStep()
		if cur >= plan.GetTotalSteps() {
			_ = s.rollouts.Complete(dep.GetName(), svc.GetName())
			return -1
		}
	}
	return cur
}

// isRollingImageChange reports whether all current replicas for the
// service share a single image that differs from the new desired image.
// Returns false when there are no current replicas (fresh deployment —
// no rollout needed) or when any current replica already runs the new
// image (rollout already in flight, just keep rolling).
func isRollingImageChange(currentByID map[string]*pb.ReplicaDesired, desiredImage string) bool {
	if len(currentByID) == 0 {
		return false
	}
	for _, r := range currentByID {
		if r.GetImage() == desiredImage {
			// At least one replica already at the new image — let the
			// remaining ones flip one-per-pass too, so behavior is
			// stable across reconciles after the rollout starts.
			return true
		}
	}
	for _, r := range currentByID {
		if r.GetImage() == "" || r.GetImage() == desiredImage {
			return false
		}
	}
	return true
}

// deploymentStatusPending builds a Command that flips a Deployment into
// status=PENDING with the reason populated in details.
func deploymentStatusPending(name, reason string) *pb.Command {
	return &pb.Command{
		Identity: "scheduler",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_DeploymentStatusUpdate{DeploymentStatusUpdate: &pb.DeploymentStatusUpdate{
			Deployment: name,
			Status:     pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING,
			Details:    map[string]string{"reason": reason},
		}},
	}
}
