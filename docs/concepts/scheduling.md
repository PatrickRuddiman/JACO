---
sources:
  - internal/scheduler/
  - internal/scheduler/rebalance/
  - internal/runtime/cgroupv2/
  - internal/runtime/reconciler/
  - internal/runtime/pull/
  - internal/runtime/lifecycle/lifecycle.go
  - internal/controlplane/grpc/jaco_spec.go
  - proto/jaco/v1/entities.proto
---

# Scheduling

The scheduler is the **leader-only** desired-state reconciler. It reads
`Deployment`, `Node`, and `ReplicaObserved` from control-plane watches
and writes `ReplicaDesired`, `RolloutPlan`, and
`Deployment.applied_revision` through raft. Followers run nothing
scheduler-side; on leader change, the new leader picks up where the
old one left off via raft state.

## Reconcile trigger

- **Watch-driven** with a 50 ms debounce — coalesces bursts of events
  during a rolling update.
- **Safety tick** every 30 s — runs reconcile unconditionally to
  recover from any missed event.

## Placement modes

Per `services[*].placement` (see
[`jaco.yaml`](../manifests/jaco-yaml.md)):

- **`spread`** (default) — `eligible = all healthy nodes`. Replica `i`
  lands on `eligible[hash(deployment+service+i) mod len(eligible)]`.
  The hash is stable: the same `(deployment, service, i)` always picks
  the same host given the same eligible set. This avoids needless
  churn on leader failover, when reconcile re-runs from scratch.
- **`pack`** — `eligible = all healthy nodes` sorted by current
  replica count (any service) ascending. Replicas pile onto the
  lowest-loaded host first. Ties broken by hostname.
- **`hosts`** — `eligible = hosts ∩ healthy_nodes`. If
  `len(eligible) < replicas`, no replicas are scheduled; the
  deployment reports `pending` with details
  `{reason: cannot_satisfy_host_placement, missing: [...]}`.
- **`global`** (daemonset) — `eligible = all healthy nodes`, and the
  scheduler places exactly one replica on each eligible host. The
  service's effective count tracks the size of the ready node set
  automatically. Setting `replicas:` together with `placement: global`
  is **rejected** at apply time with `service "x" uses
  placement=global; remove replicas (global runs one replica per ready
  node)` (issue #99) — operators are made to remove the count rather
  than seeing the scheduler silently ignore it. Replica ids are
  derived from the hostname rather than a positional index, so a
  surviving node's replica id is stable when other nodes join or
  leave — no churn.

## Replica ids

Replicas are named `<deployment>-<service>-<index>` for `spread`,
`pack`, and `hosts`. The index is a per-service monotonic counter
(`ReplicaCounter`) stored in raft; indices are never reused. For
`global`, ids are `<deployment>-<service>-<host>`: hostname-keyed so a
node's replica id survives membership churn. All forms are stable,
sortable, and grep-friendly in logs.

## Rolling updates

Triggered by any change to a service's image, replicas count, or
placement when `replicas > 1`. The scheduler creates a `RolloutPlan`
in raft (`total_steps = replicas`, `current_step = 0`, state
`in_progress`) and advances one step at a time:

- Step `k`: pick the `k`th replica (by index), write
  `ReplicaDesired{id: <dep>-<svc>-<k>, image: target}`, advance
  `current_step = k+1`. Wait for `ReplicaObserved{state: running,
  last_health_at recent}` on that replica before continuing.
- **Invariant**: at no step is more than one replica in the service
  simultaneously not-running, excluding the one being replaced (which
  is stopped only when its replacement is healthy).
- **Step timeout**: 60 s default per step. Exceeding it aborts the
  rollout (`RolloutPlan.state = aborted`, `failure_reason` set). The
  previous successful revision keeps serving — JACO does not strand
  half-rolled state.

`jaco status -w` shows the rollout state moving through the steps.
[`jaco rollback`](../cli/rollback-delete.md) reverts to
`previous_revision` if you don't want to wait or want to abandon a bad
roll.

## Replica start and image-pull visibility

The runtime reconciler on each node starts a desired replica
**asynchronously**: the image pull, container create, and start run in
a goroutine, so a slow or stuck pull on one replica never freezes the
reconcile loop or stalls the other replicas on that node. Starts are
deduped by replica id — a re-dispatch for the same desired `raft_index`
is a no-op, while a changed `raft_index` (e.g. an image roll) cancels
and supersedes the in-flight start.

Image-pull progress is surfaced as `ReplicaObserved` so a stuck pull is
visible in [`jaco status`](../cli/status.md) and the node logs rather
than silently retrying forever:

- First attempt → `ReplicaObserved{state: pulling, code: pulling}`.
- Each failed attempt → `ReplicaObserved{state: pending, code:
  image_pull_failed, details: {reason: "<image>: <error>"}}`, plus a
  `warn` line in the node log (`image pull failed; will retry`). The
  pull keeps retrying; the replica sits in `pending` with the
  `image_pull_failed` code attached until it succeeds.

This is distinct from the terminal `failed` state, which the scheduler
sets only after the restart budget is exhausted (below). A replica
whose subnet allocation cannot be satisfied is published directly as
`ReplicaObserved{state: failed, code: subnet_pool_exhausted}`.

## Health and restart policy

The compose `restart:` field is intentionally **ignored** — the
scheduler owns restart decisions cluster-wide. On a replica's
`ReplicaObserved.state` transitioning to `degraded` (healthcheck
failing past the compose threshold):

1. Scheduler writes a `ReplicaCommand{op: remove_from_routing}`;
   ingress drops the replica from its upstream pool within 5 s.
2. Scheduler writes a `ReplicaCommand{op: restart}`; runtime stops
   the container (sending the configured `stop_signal` and waiting up
   to the configured `stop_grace_period` before SIGKILL — see
   [shutdown semantics](#shutdown-semantics) below) and starts a fresh
   one with the same spec.
3. The `RestartCounter` for the replica increments. On a
   subsequent `running` observation, the counter is cleared.
4. **After 3 consecutive failures** without an intervening `running`,
   the scheduler writes `ReplicaObserved{state: failed, code:
   restart_exhausted}` and stops issuing restarts for that replica
   until the next `Deploy.Apply`.

## Shutdown semantics

`runtime/lifecycle.Stop` honors compose's `stop_signal` and
`stop_grace_period` (issue #114). Both are persisted on the container
by docker (`Config.StopSignal`, `Config.StopTimeout`); the lifecycle
stop path sends the configured signal and waits up to the configured
grace before issuing SIGKILL. Calling `Stop` with a negative grace
(the runtime reconciler's default) defers to the persisted value, so
each service gets its own deadline without the reconciler tracking
per-service specs.

Pre-issue #114, every service used a hardcoded SIGTERM and a 10 s
grace regardless of the compose declaration — Postgres, Redis, nginx,
and Kafka all silently lost data on `jaco rm` / replica rotation when
10 s wasn't enough to flush. Operators porting compose stacks should
confirm `stop_grace_period` is declared on stateful services.

## Drain on graceful node remove

When `jaco node remove <hostname>` runs, the drain machine:

1. Enumerates `ReplicaDesired` where `host = <hostname>`.
2. For each replica, computes a new host using the placement rules on
   the remaining eligible set, writes
   `ReplicaDesired{id, host: new_host}`. The old replica on the
   leaving host remains running until the new one passes health.
   Exception: **`placement: global`** replicas are not migrated —
   they are simply dropped, because the daemonset already runs one
   replica per node and migrating would double-place it on the target.
3. When every replacement reports `running`, writes
   `ReplicaDesired{id, host: removed}` for the old replicas; runtime
   on the leaving node stops + removes them.
4. Issues `Cluster.NodeRemove`; FSM removes the `Node` entity.

Per-replica drain timeout is 5 minutes; on timeout, the node remove
aborts with `pending: drain_timeout`.

## Reconcile pass (one cycle)

1. Snapshot current state from in-memory typed stores (Deployments,
   healthy Nodes, ReplicaDesired, ReplicaObserved).
2. For each `Deployment.service`:
   a. Compute the desired replica set
      `D = {(id, host) for i in 0..replicas-1}` via placement.
   b. Diff against current `ReplicaDesired` for that service: adds,
      removes, host-moves, image-changes.
   c. If an image change + `replicas > 1` + no active
      `RolloutPlan`: start one and execute step 0.
   d. If an active `RolloutPlan` exists: check whether the current
      step's replica reports `running` with recent health; advance to
      `current_step + 1`; complete when `current_step ==
      total_steps`.
3. Submit all required mutations as one batched raft `Apply` so the
   apply-to-steady-state stays under the 15 s bar.

## Per-service spec hash (drift detection)

`ReplicaDesired.spec_hash` carries a SHA-256 of the canonical
per-service slice of the resolved compose YAML (the `services.<name>`
subtree, decoded then JSON-marshalled with sorted keys via
[`compose.ServiceSpecHash`](../../internal/runtime/compose/spec_hash.go)).
The scheduler computes it on every pass and includes it in the upsert
gate:

```go
// internal/scheduler/scheduler.go
if cur.GetHost() == host &&
   cur.GetImage() == image &&
   bytes.Equal(cur.GetSpecHash(), specHash) {
    continue   // no upsert; container stays as-is
}
```

A change in env values, healthcheck command, mounts, labels, or any
other compose field flips the hash and fires a
`ReplicaDesiredUpsert`. The FSM bumps `RaftIndex`, the runtime
reconciler observes the mismatch via `lifecycle.Start`'s
`matchesRaftIndex` check, and the container is stop+removed+created
with the new spec baked in at create time.

Pre-v0.3.1 the upsert gate compared only `(Host, Image)`. Any other
compose change — most painfully a `.env` rotation that changed env
VALUES under the same env-var KEYS — yielded `continue` → no upsert
→ container reused with the stale env baked at the previous create
(container env is immutable for the life of a container). The only
escape was `docker rm -f` per stuck replica. Issue #148.

The canonical form deliberately strips comments and reformatting:
adding a `# explain this` comment to your compose file does **not**
flip the hash, while semantic edits (env-value, healthcheck, mount
path) do. The cosmetic-stability is pinned by `TestServiceSpecHash_StableUnderCosmeticEdits`.

First post-upgrade scheduler pass on a cluster running pre-v0.3.1
replicas computes the hash and finds it empty on every desired
replica → emits upsert → recreates each container once. Acceptable
rolling-deploy churn; the alternative was silent drift.

## Quotas

Per-replica CPU/memory limits are enforced by the runtime (compose
`deploy.resources` + the legacy top-level keys; see
[compose.md](../manifests/compose.md)). The placement scheduler is
not capacity-aware: it does not model node capacity or per-service
resource requests, so a `pack` placement can overcommit a node if the
operator asks for it. The pressure-based rebalancer (below) is the
feedback loop that catches the over-packed case at runtime.

IO/block-device limits and autoscaling are explicitly out of scope
for v1.

## Pressure-based rebalancing

The leader runs a second, **independent** scheduler loop: the
`internal/scheduler/rebalance` package
([ADR 0002](../adr/0002-pressure-based-scheduling.md), issue #92).
It observes per-node CPU + memory pressure, picks the cheapest
stateless replica to move off the hottest node when the cluster is
meaningfully imbalanced, and emits a single move per cycle through
the existing scheduler raft-Apply path
(`ReplicaDesiredUpsert` with a new `Host`). The runtime reconciler
on the source node then stops the container, and the new host's
reconciler pulls the image and starts it — same code path as any
other placement change.

The rebalancer is **always-on** and has **no operator-facing config
block**. Defaults bake in; tight gates bias so strongly toward
inaction that an idle or uniformly-busy cluster never moves anything.

### Pressure signal

Every daemon samples its local cgroup v2 + `/proc/meminfo`
utilisation on the `node_status_interval` cadence (default 30 s,
range 5 s..5 m; see [Configuration](../configuration.md)) and
gossips a `NodeStatusUpdate{IncludePressure: true}` through raft.
The leader's `StateBackedSource` reads each node's latest sample
out of state, gated on a freshness window of 3× the heartbeat
interval — a crashed node stops influencing decisions within a
couple of intervals.

The cycle loop folds each sample into a per-node EWMA with a 5-minute
continuous-time window, so the trigger fires on **sustained drift**,
not on instantaneous spikes. Composite pressure for the gates is
`max(cpu_ewma, memory_ewma)` — two dimensions, by design; disk-io and
replica-count were considered and cut.

### Trigger gates (all must hold)

A move commits only when **every** check passes for the candidate
`(replica, src, dst)`:

| gate                | default                    | what blocks it                                                                |
|---------------------|----------------------------|-------------------------------------------------------------------------------|
| trigger threshold   | `max(pressure) ≥ 0.85`     | hot node has been hot for the last `consecutive_cycles` cycles                |
| consecutive cycles  | 2 (≈1 min at 30 s tick)    | a transient single-tick spike                                                 |
| imbalance gap       | `max − min ≥ 0.25`         | the whole cluster is uniformly busy — no cooler host to move to               |
| relief floor        | `pre_src − post_src ≥ 0.10`| the chosen replica's footprint is too small to materially relieve src         |
| dst cap             | `post_dst < 0.75`          | the move would push dst into the trigger band — relocating the hotspot       |
| dst resource fit    | `post_cpu + post_mem ≤ 1.0`| dst would over-saturate one dimension after the move                          |
| anti-affinity       | per `placement` mode       | SPREAD collision on dst, HOSTS dst not in spec.Hosts, GLOBAL never moves      |
| replica cooldown    | 10 min                     | replica was moved (or placed) recently — refuses to ping-pong it              |
| node cooldown       | 2 min                      | dst received a move recently — staggers convergence                           |

These bake into `internal/scheduler/rebalance/config.Config`. Tests
inject a `Config` to drive deterministic cycles; production
constructs `DefaultConfig()` once at boot.

### Replica eligibility

The rebalancer enumerates candidates from `ReplicasDesired` on the
hot node and applies the hard filters above before scoring. Notable
exclusions:

- **Stateful replicas** are not candidates. JACO has no networked
  storage, so moving a replica whose service spec carries a bind
  mount or named-volume reference would strand its data on the old
  host. The scorer is stateless-only by construction.
- **`placement: global`** replicas are never moved — daemonsets are
  one-per-host by definition and migration would double-place them
  on the target.
- A replica whose **deployment / service spec is missing** (deleted
  out from under it) is treated as having no anti-affinity
  constraint — the rebalancer can still move it if a dst exists.

### Scoring

Among the survivors, the winner is the **cheapest move that buys the
most relief**:

```
score(replica) = relief_estimate(replica) − move_cost
```

- `relief_estimate` is the replica's contribution to src's **dominant**
  pressure dimension (CPU-dominant hotspot → replica CPU footprint;
  memory-dominant → memory footprint). The footprint comes from the
  declared per-replica limit in the service spec when available; a
  workload without declared limits falls back to a conservative
  default (`CPU 0.12, mem 0.06`) sized just above the relief floor so
  a single move estimates to clear it.
- `move_cost` is a small constant (`0.01`) so ties break
  deterministically by replica id.

### Convergence rate

**At most one move per cycle, cluster-wide.** A single move lands,
the next cycle re-evaluates against the new state. Slower
convergence, no avalanche. Cycle cadence is `cycle_interval`
(default 30 s, same as the heartbeat).

### Decision authority

The rebalancer runs on every daemon but `Cycle` self-gates on
`LeaderStatus.IsLeader()` (same pattern as the placement scheduler) —
followers tick the loop but commit nothing. The leader-local state
the gates need (per-node EWMAs, cooldown timestamps, consecutive-over
counters) is intentionally **not** in raft: losing it on a failover
is fine, because the new leader rebuilds it within two cycles of
fresh pressure samples. The decision itself (the move command) goes
through raft like any other placement change, so `jaco status` and
the audit log reflect committed moves.

### Observability

Every committed move writes one
`rebalance_moved` audit event; every per-candidate skip writes one
`rebalance_skipped`. Both carry the same payload shape:

```
replica_id, deployment, service, src, dst,
dominant (cpu|memory),
relief, score, move_cost,
src_pressure_before, dst_pressure_before,
src_pressure_after,  dst_pressure_after,
reason  (only on SKIPPED — see below)
```

`reason` is one of: `cooldown_replica`, `cooldown_node`, `dst_cap`,
`relief_floor`, `resource_limits`, `anti_affinity`, `no_eligible_dst`,
`no_candidate`. Audit field number `22` is reserved for the
now-removed `rebalance_dry_run` type (the rebalancer was simplified
to always-on; the tag stays reserved so historical blobs decode).

Tail decisions live:

```sh
jaco audit --server $LEADER -f --type rebalance_moved,rebalance_skipped
```

### What makes the rebalancer effectively dormant today

The daemon currently wires the rebalancer with a real cgroup v2
collector + `StateBackedSource`, so the gates fire as designed on
Linux hosts where cgroup v2 is mounted and readable. On non-Linux dev
hosts, in a container without cgroup access, or while a node has not
yet emitted its first pressure sample, the freshness gate drops the
node from scoring — the cycle still runs, gates short-circuit on "no
data this cycle", and zero audit events are produced. This is the
designed posture: absence is treated as silence, not as a hard fault.

The rebalancer never moves a stateful workload, and there is no
per-deployment opt-out — operators who need a service immovable use
a pinned `placement: hosts` or attach a volume.

## GPU workloads

Compose's `gpus:` field (issue #116) is honored: a request like
`gpus: all` or a long-form list with `driver`/`count`/`capabilities`
forwards onto docker's `HostConfig.DeviceRequests`. **JACO does not
install or manage the device-runtime hook** — the operator is
responsible for deploying nvidia-container-runtime (or the AMD
equivalent) on every node that should host GPU workloads. The
scheduler does not currently inspect node-level GPU inventory; until
it does, an `gpus:`-using deployment can land on a CPU-only node and
the docker create call will fail with the daemon's "could not
select device driver" error. Pin GPU services to GPU nodes via
`jaco.yaml`'s placement rules in the meantime.

## See also

- [`jaco apply`](../cli/apply.md), [`jaco status`](../cli/status.md),
  [`jaco rollback`](../cli/rollback-delete.md)
- [`jaco.yaml` schema](../manifests/jaco-yaml.md)
- [Status and errors](status-and-errors.md)
