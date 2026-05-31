package rebalance

import (
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
)

// StateBackedSource is the production PressureSource the daemon wires
// once a real cgroup collector ships per-node samples. It reads
// Node.{CpuPressure,MemoryPressure,LastPressureAt} from the raft FSM —
// every daemon's heartbeat publishes its own sample, the leader reads
// them all out via this source.
//
// Freshness gate: samples older than MaxAge are treated as missing
// (the rebalancer skips that node this cycle). The daemon sets MaxAge
// to roughly 3× the configured heartbeat interval — long enough to
// tolerate a missed cycle, short enough that a crashed node stops
// influencing decisions within a couple of intervals.
//
// ReplicaFootprint is the conservative "declared limit, else
// just-above-the-relief-floor default" fallback documented in #137:
// the leader doesn't gossip per-replica samples (raft volume + RPC
// fanout both rejected as premature) and the rebalancer's scorer
// gates moves on relief ≥ ReliefFloor. The default lands at 0.12 CPU
// so a single moved replica clears that floor — anything lower would
// freeze the rebalancer on every workload that hasn't declared
// per-replica limits.
type StateBackedSource struct {
	State  *state.State
	MaxAge time.Duration
	Now    func() time.Time

	// FootprintFor returns the declared resource footprint for a
	// replica id. Wired by the daemon to the scheduler's declared-
	// limits table; left nil for tests, in which case every replica
	// reports defaultFootprint.
	FootprintFor func(replicaID string) (cpu, mem float64)
}

// NodePressure reads the latest gossiped sample for the named node
// out of raft state. Returns ok=false when the node is unknown, when
// no sample has landed yet, or when the most recent sample is older
// than MaxAge.
func (s *StateBackedSource) NodePressure(host string) (Snapshot, bool) {
	node, ok := s.State.Nodes.Get(host)
	if !ok {
		return Snapshot{}, false
	}
	ts := node.GetLastPressureAt()
	if ts == nil {
		return Snapshot{}, false
	}
	if s.MaxAge > 0 {
		now := time.Now
		if s.Now != nil {
			now = s.Now
		}
		if now().Sub(ts.AsTime()) > s.MaxAge {
			return Snapshot{}, false
		}
	}
	return Snapshot{
		CPU:    node.GetCpuPressure(),
		Memory: node.GetMemoryPressure(),
	}, true
}

// ReplicaFootprint returns the declared-limits footprint for a
// replica, or defaultFootprint when the lookup is unwired or the
// replica has no declared limits.
func (s *StateBackedSource) ReplicaFootprint(replicaID string) Footprint {
	if s.FootprintFor == nil {
		return defaultFootprint
	}
	cpu, mem := s.FootprintFor(replicaID)
	if cpu == 0 && mem == 0 {
		return defaultFootprint
	}
	return Footprint{CPU: cpu, Memory: mem}
}

// defaultFootprint is what the source reports for a replica with no
// declared limits. CPU sits just above the DefaultConfig ReliefFloor
// (0.10) so a single moved replica is estimated to clear the floor
// and a move can win on its own. Smaller-than-floor would freeze the
// rebalancer on every workload without declared per-replica limits,
// defeating the whole point of #137.
var defaultFootprint = Footprint{CPU: 0.12, Memory: 0.06}
