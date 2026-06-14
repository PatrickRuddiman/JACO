package grpcsrv

import (
	"context"
	"sort"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// GetReplicas returns the rich per-replica view backing `jaco get replica[s]`
// (issue #175). It joins the desired spec (deployment, service, image,
// revision), the latest observation (state, reason, container), the restart
// counter, and the resolved compose `depends_on` gates for each replica.
// Reads local state only; no leader required so the CLI can hit any node,
// mirroring Status.
func (d *deployServer) GetReplicas(_ context.Context, req *pb.GetReplicasRequest) (*pb.GetReplicasResponse, error) {
	depFilter := req.GetDeploymentFilter()
	svcFilter := req.GetServiceFilter()
	idFilter := req.GetReplicaId()

	// Index observations + restart counters by replica id so the per-desired
	// lookup stays O(1).
	observedByID := map[string]*pb.ReplicaObserved{}
	for _, o := range d.state.ReplicasObserved.List() {
		observedByID[o.GetId()] = o
	}
	restartByID := map[string]*pb.RestartCounter{}
	for _, rc := range d.state.RestartCounters.List() {
		restartByID[rc.GetReplicaId()] = rc
	}

	// Cache parsed compose depends_on per (deployment, service) so a fan-out
	// of replicas for the same service parses the compose document once.
	dependsCache := map[string][]compose.Dependency{}

	resp := &pb.GetReplicasResponse{}
	for _, rd := range d.state.ReplicasDesired.List() {
		if idFilter != "" && rd.GetId() != idFilter {
			continue
		}
		if depFilter != "" && rd.GetDeployment() != depFilter {
			continue
		}
		if svcFilter != "" && rd.GetService() != svcFilter {
			continue
		}

		detail := &pb.ReplicaDetail{
			Id:         rd.GetId(),
			Deployment: rd.GetDeployment(),
			Service:    rd.GetService(),
			Index:      rd.GetIndex(),
			Host:       rd.GetHost(),
			Image:      rd.GetImage(),
			Revision:   rd.GetRaftIndex(),
		}

		if o, ok := observedByID[rd.GetId()]; ok {
			detail.State = o.GetState()
			detail.Code = o.GetCode()
			detail.Message = o.GetMessage()
			detail.Details = o.GetDetails()
			detail.ContainerId = o.GetContainerId()
			// Prefer the reporting host from the observation when present;
			// it reflects where the container actually landed.
			if h := o.GetHost(); h != "" {
				detail.Host = h
			}
			detail.StartedAt = o.GetStartedAt()
			detail.LastHealthAt = o.GetLastHealthAt()
		}

		if rc, ok := restartByID[rd.GetId()]; ok {
			detail.RestartCount = rc.GetConsecutiveFailures()
			detail.LastAttemptAt = rc.GetLastAttemptAt()
		}

		detail.DependsOn = d.resolveDependsOn(rd.GetDeployment(), rd.GetService(), dependsCache)

		resp.Replicas = append(resp.Replicas, detail)
	}

	// Stable order: deployment, service, index — so list output and golden
	// tests are deterministic regardless of map iteration order.
	sort.Slice(resp.Replicas, func(i, j int) bool {
		a, b := resp.Replicas[i], resp.Replicas[j]
		if a.GetDeployment() != b.GetDeployment() {
			return a.GetDeployment() < b.GetDeployment()
		}
		if a.GetService() != b.GetService() {
			return a.GetService() < b.GetService()
		}
		if a.GetIndex() != b.GetIndex() {
			return a.GetIndex() < b.GetIndex()
		}
		return a.GetId() < b.GetId()
	})
	return resp, nil
}

// resolveDependsOn returns the resolved start-ordering dependencies for one
// (deployment, service): each compose-declared depends_on target paired with
// that target service's current aggregate observed state and whether the gate
// is satisfied right now. cache memoizes the compose parse per service.
func (d *deployServer) resolveDependsOn(deployment, service string, cache map[string][]compose.Dependency) []*pb.DependencyState {
	deps := d.dependenciesFor(deployment, service, cache)
	if len(deps) == 0 {
		return nil
	}
	out := make([]*pb.DependencyState, 0, len(deps))
	for _, dep := range deps {
		state := d.bestObservedState(deployment, dep.Service)
		out = append(out, &pb.DependencyState{
			Service:   dep.Service,
			Condition: dep.Condition,
			State:     state,
			Satisfied: dep.Required && dependencySatisfied(state, dep.Condition),
		})
	}
	return out
}

// dependenciesFor parses the deployment's stored compose document and returns
// the depends_on list for the named service, memoized in cache. A missing
// deployment, unparseable compose, or service without depends_on yields nil.
func (d *deployServer) dependenciesFor(deployment, service string, cache map[string][]compose.Dependency) []compose.Dependency {
	key := deployment + "\x00" + service
	if cached, ok := cache[key]; ok {
		return cached
	}
	var deps []compose.Dependency
	if dep, ok := d.state.Deployments.Get(deployment); ok {
		if raw := dep.GetComposeYaml(); len(raw) > 0 {
			if project, err := compose.LoadBytes(raw, "compose.yml"); err == nil {
				if svc, err := project.GetService(service); err == nil {
					deps = compose.ToContainerSpec(svc, compose.SpecOptions{
						Deployment: deployment,
						Service:    service,
					}).DependsOn
				}
			}
		}
	}
	cache[key] = deps
	return deps
}

// bestObservedState reports the most-advanced observed state across every
// replica of (deployment, service): RUNNING if any replica is running, else
// the highest-ranked reported state, or UNSPECIFIED when none have reported.
// Used to summarize a dependency service's health for `jaco get replica`.
func (d *deployServer) bestObservedState(deployment, service string) pb.ReplicaState {
	best := pb.ReplicaState_REPLICA_STATE_UNSPECIFIED
	for _, rep := range d.state.ReplicasDesired.List() {
		if rep.GetDeployment() != deployment || rep.GetService() != service {
			continue
		}
		obs, ok := d.state.ReplicasObserved.Get(rep.GetId())
		if !ok {
			continue
		}
		if stateRank(obs.GetState()) > stateRank(best) {
			best = obs.GetState()
		}
	}
	return best
}

// dependencySatisfied mirrors the reconciler's depends_on gate (issue #130):
// service_started is met by RUNNING or DEGRADED (the container has been run);
// service_healthy requires RUNNING. Kept in sync with
// internal/runtime/reconciler/depends_on.go so status and the gate agree.
func dependencySatisfied(state pb.ReplicaState, condition string) bool {
	switch condition {
	case compose.DependencyConditionHealthy:
		return state == pb.ReplicaState_REPLICA_STATE_RUNNING
	case compose.DependencyConditionStarted, "":
		return state == pb.ReplicaState_REPLICA_STATE_RUNNING ||
			state == pb.ReplicaState_REPLICA_STATE_DEGRADED
	}
	return false
}

// stateRank orders replica states from least to most advanced toward a
// healthy running container, so bestObservedState can pick the representative
// state for a dependency service. RUNNING outranks everything; terminal
// failure states rank low so a single running replica still reads as running.
func stateRank(s pb.ReplicaState) int {
	switch s {
	case pb.ReplicaState_REPLICA_STATE_RUNNING:
		return 7
	case pb.ReplicaState_REPLICA_STATE_DEGRADED:
		return 6
	case pb.ReplicaState_REPLICA_STATE_UPDATING:
		return 5
	case pb.ReplicaState_REPLICA_STATE_PULLING:
		return 4
	case pb.ReplicaState_REPLICA_STATE_PENDING:
		return 3
	case pb.ReplicaState_REPLICA_STATE_STOPPED:
		return 2
	case pb.ReplicaState_REPLICA_STATE_FAILED:
		return 1
	default:
		return 0
	}
}
