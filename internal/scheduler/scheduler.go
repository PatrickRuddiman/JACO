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
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/counter"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/placement"
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

	mu     sync.Mutex
	active bool
	cancel context.CancelFunc
}

// New constructs a Scheduler.
func New(s *state.State, brokers *watch.Registry, leader LeaderStatus, apply Applier) *Scheduler {
	return &Scheduler{state: s, brokers: brokers, leader: leader, apply: apply}
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

	deployments := s.state.Deployments.List()
	nodes := s.state.Nodes.List()

	var batch []*pb.Command

	for _, dep := range deployments {
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
		return
	}
	_ = s.apply(data)
}

// reconcileService computes the diff between current and desired
// ReplicaDesired for one service. Returns the Command list (may be empty
// when current already matches desired).
func (s *Scheduler) reconcileService(dep *pb.Deployment, svc *pb.ServiceSpec, nodes []*pb.Node, project *composeProject) []*pb.Command {
	image := lookupImage(project, svc.GetComposeService())
	if image == "" {
		return []*pb.Command{deploymentStatusPending(dep.GetName(),
			fmt.Sprintf("service %q references unknown compose_service %q", svc.GetName(), svc.GetComposeService()))}
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
			Payload: &pb.Command_ReplicaDesiredRemove{ReplicaDesiredRemove: &pb.ReplicaDesiredRemove{Id: id}},
		})
	}

	return cmds
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
