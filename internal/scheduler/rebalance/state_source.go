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
// ReplicaFootprint is the conservative "declared limit, else tiny
// default" fallback documented in #137: the leader doesn't gossip
// per-replica samples (raft volume and RPC fanout both rejected as
// premature) and the rebalancer's scorer is already conservative
// enough that a slightly-pessimistic footprint biases toward "don't
// move", which is the safe direction.
type StateBackedSource struct {
	State  *state.State
	MaxAge time.Duration
	Now    func() time.Time

	// FootprintFor returns the declared resource footprint for a
	// replica id. Wired by the daemon to the scheduler's declared-
	// limits table; left nil for tests, in which case every replica
	// reports a tiny default footprint that won't trigger moves on
	// its own.
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
// replica, or a tiny default if the lookup is unwired or the
// replica has no declared limits. The default biases toward
// under-estimating relief, which the scorer's `relief - 0.01` floor
// turns into "don't move" — the conservative direction.
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
// declared limits: 5% of a single CPU and 2% of node memory. Small
// enough that one unknown replica never tips the relief estimate
// above the SkipReliefFloor on its own.
var defaultFootprint = Footprint{CPU: 0.05, Memory: 0.02}
