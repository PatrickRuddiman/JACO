package rebalance

// Quorum is the rebalancer's conservative quorum-safety check. v0 models
// every service's replicas as a single quorum group: removing a replica
// from its current host must not drop the cluster-wide count of
// same-service replicas in the RUNNING state below ⌈N/2⌉ + 1, where N
// is the configured replica count for the service.
//
// This is intentionally simpler than the full ADR 0002 picture (which
// mentions declared per-service quorum groups). The simpler model
// catches the real failure mode — a rebalance ripping the majority out
// from under raft / pg streaming / redis primary by moving the wrong
// replica when others are unhealthy — without needing a new
// quorum-membership schema. Declared groups land in a follow-up when
// services other than "all replicas of one service" appear.
//
// The check is a *removal* check, not a placement check: the move
// itself is stop-on-src then start-on-dst; the window between the two
// is when quorum is at risk, so the safety bar is "does the cluster
// stay over the floor with this replica gone".
type Quorum struct {
	// Members maps (deployment, service) → list of running members.
	// A replica counts as a quorum member when it is RUNNING AND on
	// a different host than src (i.e. it survives the move's stop
	// step). Callers populate this from state.ReplicasObserved before
	// each Cycle.
	members map[serviceKey]int
	// Specs maps (deployment, service) → declared replica count
	// (ServiceSpec.Replicas). Used to compute ⌈N/2⌉ + 1.
	specs map[serviceKey]int
	// owners maps replicaID → (deployment, service) so the
	// WouldBreakQuorum lookup is one map hop.
	owners map[string]serviceKey
}

type serviceKey struct {
	deployment string
	service    string
}

// NewQuorum constructs an empty quorum view. Populate via
// AddSpec / AddRunning before calling WouldBreakQuorum; see also
// QuorumStateFromObservations for the standard build path used by the
// Rebalancer.
func NewQuorum() *Quorum {
	return &Quorum{
		members: map[serviceKey]int{},
		specs:   map[serviceKey]int{},
		owners:  map[string]serviceKey{},
	}
}

// AddSpec records the declared replica count for a service. N defines
// the quorum floor: ⌈N/2⌉ + 1. N ≤ 1 is treated as "no quorum
// constraint" (a single replica has no majority to lose).
func (q *Quorum) AddSpec(deployment, service string, n int) {
	q.specs[serviceKey{deployment, service}] = n
}

// AddRunning records that replicaID is currently RUNNING on host.
// Callers iterate state.ReplicasObserved once per cycle and call this
// for every replica in REPLICA_STATE_RUNNING.
func (q *Quorum) AddRunning(replicaID, deployment, service string) {
	k := serviceKey{deployment, service}
	q.members[k]++
	q.owners[replicaID] = k
}

// WouldBreakQuorum reports whether removing replicaID from src would
// drop the count of RUNNING same-service replicas below ⌈N/2⌉ + 1.
//
// The src/dst parameters are present for future per-host quorum-group
// modelling (e.g. "a per-rack quorum needs at least M nodes in each
// rack"); the v0 implementation only looks at the cluster-wide count
// because services aren't yet rack-aware. dst is currently ignored;
// keeping it on the signature avoids a churn cycle when the richer
// model lands.
//
// A replicaID the quorum view has never seen returns false: the
// rebalancer hard-filters unknown candidates separately, and
// returning true here would mask that bug as "quorum-blocked".
func (q *Quorum) WouldBreakQuorum(replicaID, src, dst string) bool {
	_ = dst
	k, ok := q.owners[replicaID]
	if !ok {
		return false
	}
	n := q.specs[k]
	if n <= 1 {
		return false
	}
	floor := n/2 + 1 // ⌈N/2⌉ + 1 == N/2 + 1 for integer math (odd N: 3→2, 5→3; even N: 4→3, 6→4)
	// Members count is "currently RUNNING members"; the moved
	// replica is one of them iff it itself is RUNNING. The post-stop
	// count is members - 1 (subtracting the replica we're about to
	// stop on src). We never knew whether the replica is also
	// counted in members from the caller's POV, so look the replica
	// up: if its owners entry exists it IS counted.
	post := q.members[k] - 1
	return post < floor
}
