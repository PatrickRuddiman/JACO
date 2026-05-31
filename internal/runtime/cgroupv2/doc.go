// Package cgroupv2 collects the node-level CPU and memory utilisation
// figures the rebalancer (internal/scheduler/rebalance) folds into its
// per-node pressure EWMA. Linux-only — every non-Linux build wires the
// no-data fallback so the daemon compiles on dev workstations.
//
// Scope is deliberately node-level: per-container footprints would
// require either gossiping a per-replica sample per cycle (high raft
// volume) or an extra RPC fan-out, and the rebalancer already gets
// usable behaviour from the conservative declared-limits footprint
// fallback in the runtime/lifecycle layer. Revisit per-container
// collection when a real workload demonstrates the leader needs
// better-than-declared-limit accuracy.
package cgroupv2
