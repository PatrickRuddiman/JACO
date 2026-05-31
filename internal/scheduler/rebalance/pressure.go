package rebalance

// PressureSource is the dependency the rebalancer reads node pressure
// signals through. Implementations live outside this package:
//   - the daemon supplies a stub (returns no data for every node) until
//     the real cgroup/dockerx collector lands as a follow-up;
//   - tests inject a fake that returns scripted snapshots.
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
	// stub source). The rebalancer treats !ok as "skip this node
	// this cycle" — it does NOT extrapolate.
	NodePressure(node string) (snap Snapshot, ok bool)

	// ReplicaFootprint returns the resource footprint of a single
	// replica, used by the scorer to estimate post-move pressure on
	// src and dst. Implementations return a zero-valued Footprint
	// (CPU=0, Memory=0, Bytes=0, Stateful=false) when nothing is
	// known about the replica; that conservatively under-estimates
	// relief and won't trigger a move on its own.
	ReplicaFootprint(replicaID string) Footprint
}

// Snapshot is one cycle's worth of per-node pressure signals. CPU /
// Memory / DiskIO are normalised utilisation in [0, 1] (1.0 = the
// node is saturated on that dimension). ReplicaCount is the current
// raft-known count of ReplicaDesired entries pinned to this node;
// ReplicaSoftCap is the node-local cap used to derive a fourth
// dimension (count/cap) so a node loaded with too many small replicas
// is treated as "pressured" even when CPU/memory are calm.
type Snapshot struct {
	CPU            float64
	Memory         float64
	DiskIO         float64
	ReplicaCount   int
	ReplicaSoftCap int
}

// Footprint is what a single replica costs on its current host, used
// by the scorer to estimate relief. CPU and Memory are normalised
// fractions of node capacity (same scale as Snapshot.CPU / .Memory) so
// `post = pre - footprint` arithmetic is meaningful. Bytes is the
// approximate volume size used by move-cost estimation for stateful
// replicas (ignored when Stateful=false). Stateful is the hard-filter
// gate: when true, the rebalancer refuses to move the replica until
// volume migration (#91) lands.
type Footprint struct {
	CPU      float64
	Memory   float64
	Bytes    int64
	Stateful bool
}

// Composite implements the ADR 0002 §"Signals" formula:
//
//	pressure(node) = max(cpu, memory, disk_io, count/cap)
//
// A zero or negative ReplicaSoftCap collapses the count dimension to
// 0 (i.e. the count term is ignored — guards against division by
// zero when a snapshot omits the cap, treating that node as
// "uncapped" for count purposes rather than crashing).
func Composite(s Snapshot) float64 {
	m := s.CPU
	if s.Memory > m {
		m = s.Memory
	}
	if s.DiskIO > m {
		m = s.DiskIO
	}
	if s.ReplicaSoftCap > 0 {
		ratio := float64(s.ReplicaCount) / float64(s.ReplicaSoftCap)
		if ratio > m {
			m = ratio
		}
	}
	return m
}

// Dominant returns which of the four dimensions is currently the largest
// in s. Used by the scorer to pick "what counts as relief": when
// CPU dominates, the moved replica's CPU footprint is the relief
// estimate; when Memory dominates, RSS is. The two non-instrumented
// dimensions (DiskIO and Count) currently fall back to "use whichever
// of CPU/Memory the footprint reports first non-zero" — the rebalancer
// has no per-replica disk-io or count "footprint", and a count-driven
// hotspot is relieved by moving exactly one replica regardless of size.
func Dominant(s Snapshot) Dimension {
	d := DimCPU
	max := s.CPU
	if s.Memory > max {
		d, max = DimMemory, s.Memory
	}
	if s.DiskIO > max {
		d, max = DimDiskIO, s.DiskIO
	}
	if s.ReplicaSoftCap > 0 {
		ratio := float64(s.ReplicaCount) / float64(s.ReplicaSoftCap)
		if ratio > max {
			d = DimCount
		}
	}
	return d
}

// Dimension names the pressure axis that is currently driving a node's
// composite score. Carried into the audit payload as the human-readable
// `dominant=<cpu|memory|disk_io|count>` field.
type Dimension uint8

const (
	DimCPU Dimension = iota
	DimMemory
	DimDiskIO
	DimCount
)

// String returns the lowercase wire form used in audit payloads.
func (d Dimension) String() string {
	switch d {
	case DimCPU:
		return "cpu"
	case DimMemory:
		return "memory"
	case DimDiskIO:
		return "disk_io"
	case DimCount:
		return "count"
	default:
		return "cpu"
	}
}

// StubSource is the no-op PressureSource the daemon wires while real
// signal collection is a follow-up. NodePressure returns ok=false for
// every node, so the rebalancer's gates never trigger and the loop
// produces no audit events. Tests should NEVER use this — they
// inject their own scripted fake; the stub exists purely so the
// daemon can boot the rebalancer subsystem under dry-run config
// without crashing on a nil source.
type StubSource struct{}

// NodePressure always returns no data.
func (StubSource) NodePressure(string) (Snapshot, bool) { return Snapshot{}, false }

// ReplicaFootprint always returns a zero footprint (CPU=0, Memory=0,
// Bytes=0, Stateful=false). With Stateful=false, the stateful hard
// filter would not reject; with CPU/Memory=0, the relief floor gate
// would. Either way, the absence of NodePressure data short-circuits
// the cycle before either gate is reached.
func (StubSource) ReplicaFootprint(string) Footprint { return Footprint{} }
