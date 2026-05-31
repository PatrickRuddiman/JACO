# ADR 0002: Pressure-based scheduling and migration

- **Status:** proposed
- **Date:** 2026-05-30
- **Issue:** #92

---


A **leader-driven, hysteretic, opt-in rebalancer** that observes per-node pressure signals already collected by the health subsystem, picks the cheapest replica to move off the hottest node when the cluster is meaningfully imbalanced, and uses the existing scheduler move path to relocate it. Stateful workloads are filtered out for now (see #135 for the future remote-mounted-volume backend that would let them move).

Default: **off**. Operators enable it cluster-wide. When off, this entire subsystem is a passive observer that emits "would-have-moved" decisions to the audit log so operators can dry-run the policy before turning it on.

## Signals

Composite **node pressure score** computed once per cycle (default 30 s) per node:

```
pressure(node) = max(
    ewma(cpu_util,        window=5m),
    ewma(memory_util,     window=5m),
    ewma(disk_io_util,    window=5m),
    replica_count / replica_soft_cap,
)
```

EWMA, not instantaneous — instantaneous CPU spikes are noise and would cause thrashing. Window of 5 minutes matches the existing health subsystem's reporting cadence.

Data sources:
- **CPU / memory / disk-io** — cgroup v2 stats already gathered by `internal/runtime/health` for per-replica health reporting. Aggregated to node level (sum across the deployment cgroups, plus a single sample of root cgroup CPU for the kernel/orchestrator overhead).
- **replica_count** — from the FSM; the leader already knows it.
- **replica_soft_cap** — node-local config (default 50), surfaced via `jaco status`.

No new collector. The health subsystem already publishes per-replica stats to the leader at the cadence the rebalancer needs.

## Thresholds and hysteresis

The rebalancer triggers when both of these are true:

1. `max(pressure) >= 0.85` for at least `2` consecutive cycles (≈1 minute of sustained pressure, not a spike)
2. `max(pressure) - min(pressure) >= 0.25` across nodes (cluster is imbalanced, not uniformly busy)

For each candidate move it estimates the post-move pressure on both src and dst using the moved replica's recent CPU/memory footprint, and **only commits the move if**:

- `post_move_pressure(src) <= max_pressure - 0.10` (meaningful relief), and
- `post_move_pressure(dst) < 0.75` (doesn't create a new hotspot), and
- the moved replica has been on src for at least `cooldown_replica = 10m` (don't move something we just moved), and
- the destination node hasn't received a migration in the last `cooldown_node = 2m` (don't pile new work on a node that's still settling).

These constants are config knobs (`scheduler.rebalance.{trigger_threshold, imbalance_gap, relief_floor, dst_cap, cooldown_replica, cooldown_node}`) with the defaults above. Defaults bias toward inaction.

## Selection policy

Among replicas on the most-pressured node, candidates are scored:

```
score(replica) = relief_estimate(replica) * stateless_bonus * priority_inverse
                 - move_cost(replica)
```

where:
- `relief_estimate` = the replica's contribution to src's dominant pressure dimension (CPU-driven hotspot → use replica CPU; memory-driven → use replica RSS)
- `stateless_bonus` = 2.0 for stateless, 1.0 for stateful (prefer cheap moves)
- `priority_inverse` = 1 / priority (don't move high-priority workloads when a low-priority candidate would relieve the same pressure)
- `move_cost` = for stateless, a small constant (restart cost). For stateful, an estimate of bytes-to-ship at ~50 MB/s over wg (volume size from `volumes` package) — this is what makes the scheduler prefer to move the small redis cache before it considers moving a 20 GB postgres data dir.

Constraints the scorer hard-filters before scoring:
- never violates resource limits at dst
- never violates anti-affinity (`placement: spread` / `placement: hosts:`)
- never moves a member of a quorum group when doing so would leave fewer than `ceil(N/2) + 1` healthy members in place (raft / pg streaming / redis primary-replica pairs each model as a quorum group)

A replica that fails any hard filter on every candidate dst is **not movable**, full stop — the rebalancer logs "no eligible destination" and moves on. Not an error.

## Move execution

- Stateless: emit a standard reschedule command through the existing `internal/scheduler/placement` path. The reconciler does stop-on-src / start-on-dst; the rebalancer just *chose* the move.
- Stateful: **permanently filtered for now**. The rebalancer **MUST** treat all stateful services as non-movable (hard filter) until JACO grows a volume backend that lets a replica re-attach its data on a different node (#135). The selection scorer keeps the `stateless_bonus` knob in place so once a remote-volume backend lands, flipping the filter off starts considering stateful candidates without further changes.

Concurrency cap: the rebalancer commits at most **one move per cycle, cluster-wide**. Defends against avalanche: one move lands, next cycle re-evaluates, another move maybe lands, etc. Slower convergence, no thrash.

## Decision authority

The rebalancer runs **only on the raft leader**. State it needs (node pressure history, per-replica EWMAs, cooldown timestamps) is leader-local — not in raft, because losing it on a failover is fine; the new leader rebuilds it from the next two cycles of health reports.

The decision itself (the move command) goes through raft like any other placement change — so when an operator reads `jaco status` they see the move was committed, not a leader-local fantasy.

## Control surface

### Config

`/etc/jaco/daemon.yaml` (or whichever file owns cluster config today):

```yaml
scheduler:
  rebalance:
    enabled: false               # default; dry-run when false (audit-only)
    trigger_threshold: 0.85
    imbalance_gap: 0.25
    relief_floor: 0.10
    dst_cap: 0.75
    cooldown_replica: 10m
    cooldown_node: 2m
    cycle_interval: 30s
```

### Observability

`jaco status` grows a `rebalance` section:

```
Rebalance: enabled, last cycle 12s ago
  Node pressure: jaco-1=0.42  jaco-2=0.91  jaco-3=0.38
  Recent decisions:
    14:02:11  move pg-replica/2  jaco-2 -> jaco-3  reason=cpu_pressure relief=0.18
    13:51:04  considered redis/1 jaco-2 -> jaco-3  skipped=cooldown_replica
```

Audit log records every committed move and (when in dry-run mode) every would-have-moved decision, with the same payload. This is how operators evaluate the policy before turning it on.

## Component changes

- `internal/scheduler/rebalance/` — new package. Owns the cycle loop, pressure aggregation, scorer, hard-filter, and emits move commands through the existing scheduler.
- `internal/scheduler/quorum.go` — new file. Models quorum groups (declared in `jaco.yaml` per stateful service or inferred from `placement: spread` patterns) and answers `WouldBreakQuorum(replica, src, dst)`.
- `internal/scheduler/placement/` — small extension to accept a "rebalance" provenance on a move so the audit log records *why* it was chosen.
- `internal/controlplane/fsm/` — no schema changes; rebalance commits the same placement-change entry the scheduler already uses.
- `internal/runtime/health/` — no changes; the data is already there.

## Tests

- `internal/scheduler/rebalance/pressure_test.go` — pressure math, EWMA decay, dimension dominance
- `internal/scheduler/rebalance/scorer_test.go` — relief estimate, stateless preference, cost estimate
- `internal/scheduler/rebalance/hysteresis_test.go` — won't trigger on single-cycle spike; won't move when post-move dst would exceed cap; respects both cooldowns
- `internal/scheduler/quorum_test.go` — never reduces quorum group below floor
- `internal/scheduler/rebalance/dryrun_test.go` — disabled mode emits audit events but no FSM entries
- Bed E2E: drive a node hot with `stress-ng`, observe a move of a stateless replica, observe steady state, verify no thrash for 10 minutes

## Acceptance

- A node driven to sustained `cpu_util > 0.85` for >1 minute triggers a move of an eligible replica to a cooler node, observable in `jaco status` and the audit log.
- Stateless rebalancing works on the 3-node bed end-to-end. Stateful is correctly skipped today; gets considered once #135 lands a volume backend.
- Under uniform load, no moves happen — hysteresis verified by a 10-minute soak with all three nodes at 0.6 pressure.
- A pressure move never lands a replica on a dst that would violate its resource limits, anti-affinity, or quorum constraints. (Property test over a fuzzed cluster state.)
- Disabling the rebalancer in config stops all moves within one cycle; re-enabling resumes within one cycle.

## Out of scope

- Predictive / ML scaling.
- Cluster autoscaling (adding/removing nodes).
- Network-pressure dimension. The current health subsystem doesn't measure per-replica network bytes; adding it is a separate signal-collection issue. The scorer's pressure formula has room for an extra dimension when that lands.
- Per-deployment opt-out beyond the existing `priority` mechanism. Operators who want a service immovable set `priority: critical` or pin it; we don't add a third knob.

## Sequencing

The stateless path lands now with stateful explicitly filtered out and the test suite asserting that. The stateful path turns on once #135 makes a stateful replica's data reachable on its new host.

## Estimated size

One medium PR for the stateless path (`rebalance` package + quorum modeling + status surface + tests + dry-run bed validation). A follow-up small PR flips the stateful filter once a volume backend (#135) is available.
