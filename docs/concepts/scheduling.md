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
  service's `replicas:` field is **ignored** and the effective count
  tracks the size of the ready node set automatically; setting a
  non-zero `replicas` together with `global` is accepted (a single
  warn-level log line records the override) but has no effect. Replica
  ids are derived from the hostname rather than a positional index, so
  a surviving node's replica id is stable when other nodes join or
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
   the container and starts a fresh one with the same spec.
3. The `RestartCounter` for the replica increments. On a
   subsequent `running` observation, the counter is cleared.
4. **After 3 consecutive failures** without an intervening `running`,
   the scheduler writes `ReplicaObserved{state: failed, code:
   restart_exhausted}` and stops issuing restarts for that replica
   until the next `Deploy.Apply`.

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

## Quotas

The scheduler is **not** capacity-aware. Per-replica CPU/memory limits
are enforced by the runtime (compose `deploy.resources` + the legacy
top-level keys; see [compose.md](../manifests/compose.md)), but the
scheduler does not model node capacity or resource requests. A pack
placement can overcommit a node if the operator asks for it.

IO/block-device limits and autoscaling are explicitly out of scope
for v1.

## See also

- [`jaco apply`](../cli/apply.md), [`jaco status`](../cli/status.md),
  [`jaco rollback`](../cli/rollback-delete.md)
- [`jaco.yaml` schema](../manifests/jaco-yaml.md)
- [Status and errors](status-and-errors.md)
