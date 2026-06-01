package reconciler

import (
	"errors"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ErrDependsOnUnmet is the sentinel runStart returns when the replica's
// declared start-ordering dependencies (issue #130) are not yet satisfied
// in cluster state. The reconciler treats it as a transient defer: the
// 30s safety tick + the ReplicasObserved watch re-dispatch the start when
// the dep transitions, so containers come up in topological order without
// the reconciler having to schedule sleeps itself.
var ErrDependsOnUnmet = errors.New("depends_on unmet; deferring start")

// checkDependsOn evaluates ContainerSpec.DependsOn against the cluster's
// observed-replica view. Returns ok=false plus the first unmet entry when
// any required dependency is not yet satisfied; ok=true with a zero
// UnmetDependency when every required dep is satisfied (or the slice is
// empty).
//
// Satisfaction rules (one per Condition):
//
//   - DependencyConditionStarted ("service_started", compose default):
//     at least one ReplicaDesired exists for (deployment, dep service) AND
//     its ReplicaObserved is in RUNNING or DEGRADED — i.e. the container
//     has actually been run on docker. PULLING does NOT satisfy: a pull
//     in progress means the container doesn't exist yet, so a dependent
//     starting now would race the dep's actual `docker run` and that
//     defeats the point of the gate.
//
//   - DependencyConditionHealthy ("service_healthy"): at least one replica
//     in RUNNING. DEGRADED does NOT satisfy — degraded means the dep failed
//     its healthcheck, and waiters for `service_healthy` chose that wait
//     explicitly because they need a healthy peer, not just a live one.
//
// Required=false entries are skipped entirely — JACO's analog of compose's
// advisory dep ("if it exists and it's healthy, great; otherwise don't
// block"). Today the field is informational only on the spec; surfacing
// advisory-but-not-satisfied deps in audit is a future iteration.
//
// Dependencies are evaluated cluster-wide, not per-host: a web replica on
// jaco-1 with `depends_on: [api]` is unblocked the moment ANY api replica
// reaches the wait condition, even if that replica lives on jaco-3. This
// matches operator expectations from compose ("api is up somewhere") and
// avoids deadlocks when the scheduler spreads dep and dependent across
// different hosts.
func checkDependsOn(st *state.State, deployment string, deps []compose.Dependency) (UnmetDependency, bool) {
	for _, d := range deps {
		if !d.Required {
			continue
		}
		if !depSatisfied(st, deployment, d.Service, d.Condition) {
			return UnmetDependency{Service: d.Service, Condition: d.Condition}, false
		}
	}
	return UnmetDependency{}, true
}

// UnmetDependency names the first unsatisfied entry checkDependsOn found.
// Carried into the runStart defer log so an operator inspecting a stuck
// replica sees exactly which dep is holding it up.
type UnmetDependency struct {
	Service   string
	Condition string
}

// depSatisfied applies a single Condition to the dep service's observed
// replicas. Walks ReplicasDesired (cheap; bounded by replica count) for
// matching (deployment, service) entries, then consults ReplicasObserved
// for each. A missing observation defaults to UNSPECIFIED, which never
// satisfies — the replica hasn't reported yet, treat it as not-started.
func depSatisfied(st *state.State, deployment, service, condition string) bool {
	for _, rep := range st.ReplicasDesired.List() {
		if rep.GetDeployment() != deployment || rep.GetService() != service {
			continue
		}
		obs, ok := st.ReplicasObserved.Get(rep.GetId())
		if !ok {
			continue
		}
		// (Cascade-delete on DeploymentDelete in the FSM ensures
		// stale observations from a prior incarnation of the same
		// replica id can't satisfy the gate after delete+re-apply.)
		if satisfies(obs.GetState(), condition) {
			return true
		}
	}
	return false
}

// satisfies maps a replica state onto a Condition. Centralised so the
// state set per condition is in one place — adding `service_healthy_for_N`
// or similar later only edits this function.
func satisfies(s pb.ReplicaState, condition string) bool {
	switch condition {
	case compose.DependencyConditionStarted:
		// "started" matches compose's semantics: the container has been
		// run on docker. RUNNING covers the healthy / no-healthcheck
		// case; DEGRADED covers the healthcheck-failing case (the
		// container IS started; healthcheck status is separate). PULLING
		// must NOT satisfy — the container hasn't been created yet, so
		// any dependent that starts now races the dep's actual `docker
		// run`. PENDING / FAILED / STOPPED / UNSPECIFIED also fail
		// because the container is absent.
		switch s {
		case pb.ReplicaState_REPLICA_STATE_RUNNING,
			pb.ReplicaState_REPLICA_STATE_DEGRADED:
			return true
		}
	case compose.DependencyConditionHealthy:
		return s == pb.ReplicaState_REPLICA_STATE_RUNNING
	}
	return false
}
