package rebalance

// PressureSource is the dependency the rebalancer reads node pressure
// signals through. Implementations live outside this package; tests
// inject a fake that returns scripted snapshots. The daemon ships a
// NoopSource today — a real cgroup v2 collector is the follow-up that
// actually makes the rebalancer fire (see issue #92 follow-up).
//
// The interface is read-only and stateless from the rebalancer's POV;
// the implementation owns any caching, sampling, or aggregation. All
// values are sampled once per cycle and immediately fed into the
// per-node EWMA in the rebalancer (so a noisy underlying sample is
// damped by the 5-minute window rather than leaking into trigger
// decisions).
type PressureSource interface {
	// NodePressure returns the most recent per-node snapshot for the
	// given hostname. The ok flag is false when the source has no
	// data for that node yet (fresh joiners, collector skew, the
	// noop source). The rebalancer treats !ok as "skip this node
	// this cycle" — it does NOT extrapolate.
	NodePressure(node string) (snap Snapshot, ok bool)

	// ReplicaFootprint returns the resource footprint of a single
	// replica, used by the scorer to estimate post-move pressure on
	// src and dst. Implementations return a zero-valued Footprint
	// (CPU=0, Memory=0) when nothing is known about the replica;
	// that conservatively under-estimates relief and won't trigger a
	// move on its own.
	ReplicaFootprint(replicaID string) Footprint
}

// Snapshot is one cycle's worth of per-node pressure signals. CPU and
// Memory are normalised utilisation in [0, 1] (1.0 = the node is
// saturated on that dimension). Only these two dimensions exist by
// design: a "simple orchestrator for a handful of nodes" rarely hits
// disk-io or replica-count hotspots that aren't already visible as CPU
// or memory pressure, and every extra dimension is another knob with
// its own failure modes. If a real need emerges (e.g. per-replica
// network bytes), grow the struct then — don't pre-build.
type Snapshot struct {
	CPU    float64
	Memory float64
}

// Footprint is what a single replica costs on its current host, used
// by the scorer to estimate relief. CPU and Memory are normalised
// fractions of node capacity (same scale as Snapshot.CPU / .Memory) so
// `post = pre - footprint` arithmetic is meaningful.
type Footprint struct {
	CPU    float64
	Memory float64
}

// Composite collapses a Snapshot to a single pressure scalar:
//
//	pressure(node) = max(cpu, memory)
//
// The scorer ranks candidates by which dimension Dominant() picks; the
// trigger / imbalance gates compare scalars across nodes.
func Composite(s Snapshot) float64 {
	if s.Memory > s.CPU {
		return s.Memory
	}
	return s.CPU
}

// Dominant names which of CPU / Memory is currently driving s's
// composite score. Used by the scorer to pick "what counts as relief":
// when CPU dominates, the moved replica's CPU footprint is the relief
// estimate; when Memory dominates, RSS is.
func Dominant(s Snapshot) Dimension {
	if s.Memory > s.CPU {
		return DimMemory
	}
	return DimCPU
}

// Dimension names the pressure axis driving a node's composite score.
// Carried into the audit payload as the human-readable `dominant=cpu|
// memory` field.
type Dimension uint8

const (
	DimCPU Dimension = iota
	DimMemory
)

// String returns the lowercase wire form used in audit payloads.
func (d Dimension) String() string {
	if d == DimMemory {
		return "memory"
	}
	return "cpu"
}

// NoopSource is the placeholder PressureSource the daemon wires while a
// real cgroup collector is the follow-up. NodePressure returns ok=false
// for every node, so the rebalancer's gates never trigger and the loop
// produces no audit events. Tests should NEVER use this — they inject
// their own scripted fake; the noop exists purely so the daemon can
// boot the rebalancer subsystem without crashing on a nil source.
type NoopSource struct{}

// NodePressure always returns no data.
func (NoopSource) NodePressure(string) (Snapshot, bool) { return Snapshot{}, false }

// ReplicaFootprint always returns a zero footprint.
func (NoopSource) ReplicaFootprint(string) Footprint { return Footprint{} }
