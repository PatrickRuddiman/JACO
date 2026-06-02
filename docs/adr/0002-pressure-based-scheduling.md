---
sources:
  - internal/scheduler/rebalance/
  - internal/runtime/cgroupv2/
  - internal/daemon/grpc/heartbeat.go
---

# ADR 0002: Pressure-based scheduling

- **Status:** accepted (simplified 2026-05-31)
- **Date:** 2026-05-30
- **Issue:** #92
- **Supersedes:** the initial draft of this ADR, which proposed a quorum-aware,
  4-dimensional, operator-gated rebalancer with stateful-volume migration
  hooks. That scope was abandoned; this revision is what actually ships.

---

A **leader-driven, hysteretic, always-on rebalancer** that observes per-node
CPU + memory pressure, picks the cheapest stateless replica to move off the
hottest node when the cluster is meaningfully imbalanced, and uses the
existing scheduler move path to relocate it.

Design priority: **conservative inaction**. JACO targets clusters of a
handful of nodes; on a 3–5 node cluster, the operator manually draining a hot
node is almost always the right answer, and an aggressive rebalancer is more
likely to flap workloads than to relieve real pressure. So the rebalancer
runs by default, but the gates are tight enough that on an idle or uniformly
busy cluster it never moves anything.

Stateful workloads are out of scope entirely. The rebalancer never moves a
replica that owns data — JACO has no way to re-attach that data on a
different host, and the orchestrator-side byte-copy alternative (issue #91)
was rejected on its own merits. Stateful here means "has a bind mount or
docker named volume that holds state the workload needs across restarts" —
the rebalancer simply never enumerates those replicas as candidates because
their service spec carries the volume reference. There is no "stateful
filter" in code; the scorer is stateless-only by construction.

## Signals

Composite **node pressure** computed once per cycle (30 s) per node:

```
pressure(node) = max(
    ewma(cpu_util,    window=5m),
    ewma(memory_util, window=5m),
)
```

EWMA, not instantaneous — instantaneous CPU spikes are noise and would cause
thrashing. Window of 5 minutes is loose enough to ignore short-lived peaks
and tight enough to react within a few minutes to a sustained imbalance.

Two dimensions, not four. Disk-io and replica-count were in the original
ADR; both were cut. Disk-io has no collector wired and a small-cluster
operator hitting disk-io saturation is almost always also hitting CPU or
memory pressure. Replica-count-vs-soft-cap is a knob nobody on a handful of
nodes will ever tune. If a third dimension genuinely earns its keep later
(e.g. per-replica network bytes), grow the struct then.

Data source: a `PressureSource` interface. The daemon currently wires a
`NoopSource` that returns "no data" for every node, so the rebalancer is
effectively dormant until a real cgroup v2 collector lands as a follow-up
(see #137). The follow-up is the only thing keeping the rebalancer from
acting today; all the decision logic, gating, and audit plumbing are live.

## Thresholds and hysteresis

The rebalancer commits a move only when **all** of these hold:

1. `max(pressure) >= 0.85` for at least `2` consecutive cycles (~1 minute).
2. `max(pressure) - min(pressure) >= 0.25` across nodes (cluster is
   imbalanced, not uniformly busy).
3. `post_move_pressure(src) <= max_pressure - 0.10` — meaningful relief.
4. `post_move_pressure(dst) < 0.75` — does not create a new hotspot.
5. The candidate replica has been on src for at least `cooldown_replica = 10m`.
6. The destination has not received a move in the last `cooldown_node = 2m`.

These are the defaults in `internal/scheduler/rebalance/config.Config` and
they bake in. There is **no operator-facing config block** — the rebalancer
is on-by-default with no knobs. Tests inject a `Config` to drive
deterministic cycles; production constructs `DefaultConfig()` once at boot.

## Selection policy

Among replicas on the most-pressured node, the cheapest move wins:

```
score(replica) = relief_estimate(replica) - move_cost
```

where:
- `relief_estimate` = the replica's contribution to src's dominant pressure
  dimension (CPU-dominant hotspot → replica CPU footprint;
  memory-dominant → replica RSS).
- `move_cost` = `0.01`, a small fixed restart penalty so ties broken by
  relief alone resolve deterministically.

No stateless/stateful bonus, no priority weighting. The scorer assumes
stateless candidates exclusively; non-stateless replicas are excluded
upstream by virtue of how the rebalancer enumerates candidates from
`ReplicasDesired`.

Constraints the scorer hard-filters before scoring:
- never violates resource limits at dst (`post_cpu`, `post_mem` both ≤ 1.0)
- never violates anti-affinity (`placement: spread` / `placement: hosts:`;
  `placement: global` is never moved by definition).

A replica that fails any hard filter on every candidate dst is **not
movable**, full stop — the rebalancer logs a `SkipNoEligibleDst` and moves
on. Not an error.

Quorum modeling was in the original ADR; it was cut. The SPREAD
anti-affinity gate already prevents the rebalancer from co-locating two
replicas of the same service, which is the actual failure mode quorum
modeling was trying to prevent for stateless raft-shaped workloads. A
genuine quorum-bearing workload is stateful and therefore not a candidate.

## Move execution

Standard reschedule command through the existing `internal/scheduler` path.
The reconciler does stop-on-src / start-on-dst; the rebalancer just *chose*
the move.

Concurrency cap: at most **one move per cycle, cluster-wide**. Defends
against avalanche — one move lands, next cycle re-evaluates. Slower
convergence, no thrash.

## Decision authority

The rebalancer runs on every node but `Cycle` self-gates on
`LeaderStatus.IsLeader()`. State it needs (pressure EWMAs, cooldown
timestamps, consecutive-over counters) is leader-local — not in raft,
because losing it on a failover is fine: the new leader rebuilds it from
the next two cycles of pressure samples.

The decision itself (the move command) goes through raft like any other
placement change, so `jaco status` reflects committed moves.

## Observability

Audit log records every committed move (`AUDIT_EVENT_TYPE_REBALANCE_MOVED`)
and every per-candidate skip
(`AUDIT_EVENT_TYPE_REBALANCE_SKIPPED`), with the same payload shape:

```
replica_id, deployment, service, src, dst,
dominant (cpu|memory),
relief, score, move_cost,
src_pressure_before, dst_pressure_before,
src_pressure_after, dst_pressure_after,
reason (only on SKIPPED: cooldown_replica | cooldown_node |
        dst_cap | relief_floor | resource_limits | anti_affinity |
        no_eligible_dst | no_candidate)
```

The `AUDIT_EVENT_TYPE_REBALANCE_DRY_RUN` tag (proto field 22) is reserved
but no longer emitted — the dry-run mode was removed when the rebalancer
became always-on.

## Component changes

- `internal/scheduler/rebalance/` — the rebalancer package. Cycle loop,
  pressure aggregation, scorer, hard-filter, audit emission.
- `internal/scheduler/placement/` — no changes (rebalancer reuses the
  existing reschedule path via raft-Apply).
- `internal/controlplane/fsm/` — no schema changes; rebalance commits the
  same `ReplicaDesiredUpsert` entry the scheduler already uses.
- `internal/daemon/grpc/server.go` — always starts the rebalancer goroutine
  with `DefaultConfig()` and `NoopSource{}`. There is no operator config
  block to wire.
- A real cgroup v2 `PressureSource` (#137) is the only follow-up work
  needed to make the rebalancer actually fire in production. Without it
  the loop spins but every gate short-circuits on "no data for this node".

## Tests

- `internal/scheduler/rebalance/pressure_test.go` — Composite math, EWMA
  decay, EWMA spike-damping, backwards-clock invariance.
- `internal/scheduler/rebalance/scorer_test.go` — relief estimate, hard
  filter ordering, anti-affinity per PlacementMode, post-move clamping.
- `internal/scheduler/rebalance/hysteresis_test.go` — single-spike does NOT
  trigger; sustained pressure DOES trigger; dst_cap / relief_floor / both
  cooldowns / imbalance_gap each block when expected; SKIPPED audit
  carries the right reason.

## Acceptance

- A node driven to sustained `cpu_util > 0.85` for >1 minute on a 2+ node
  cluster with at least 0.25 cross-node imbalance triggers a move of an
  eligible stateless replica to a cooler node, observable in the audit
  log.
- Under uniform load, no moves happen.
- A move never lands a replica on a dst that would violate its resource
  limits or anti-affinity (verified by `TestHardFilter_OrderingAndReasons`).
- The rebalancer subsystem starts on every daemon with no operator config
  and produces zero audit events on a noop source (verified by the daemon
  grpc test suite, which boots a daemon without configuring rebalance).

## Out of scope

- Stateful workloads. The rebalancer does not move replicas with attached
  volumes; #91 was rejected and #135 (remote-mounted volumes) is labeled
  wontfix-candidate.
- Disk-io and replica-count pressure dimensions. CPU + memory only.
- Operator-tunable config knobs. The defaults bake in.
- Cluster autoscaling and predictive scaling.
- Network-pressure dimension. The current health subsystem doesn't measure
  per-replica network bytes; if it ever does, add a dimension then.
- Per-deployment opt-out. Operators who want a service immovable use a
  pinned `placement: hosts:` or attach a volume.
