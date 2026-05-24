Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — scheduler

## §1 Summary

Leader-only desired-state reconciler. Reads Deployments, Nodes, and ReplicaObserved from control-plane watches; writes ReplicaDesired, RolloutPlan, and Deployment.applied_revision. Owns placement, rolling-update orchestration, drain-and-replace on graceful node remove, and the restart-with-fail-after-3 policy for health-check failures.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Reconcile trigger.** Options: watch + 30s safety tick, watch only, periodic tick only. **Chosen:** watch-driven with 50ms debounce + 30s safety tick. Rationale: meets the spec's sub-second reaction targets while protecting against missed-event drift.
2. **Spread placement tiebreaker.** Options: hash-mod by replica id, least-loaded host, round-robin. **Chosen:** `hash(deployment+service+replica_index) mod eligible_hosts`. Rationale: stable across reconciles (same input → same host); avoids needless churn on leader failover.
3. **Rollout state location.** Options: raft-stored `RolloutPlan`, in-memory only, implicit-derived. **Chosen:** raft-stored `RolloutPlan` per deployment+service. Rationale: survives leader failover; gives `jaco status` its `updating` state directly; ~N raft writes per N-replica rollout is fine at orchestrator scale.
4. **Replica id format.** Options: `<deployment>-<service>-<index>` monotonic, UUID, random suffix. **Chosen:** `<deployment>-<service>-<index>` with a per-service monotonic counter stored in raft (never reused). Rationale: human-readable, sortable, grep-friendly in logs.

## §4 Contracts & shapes

Module layout under `internal/scheduler/`:

- `internal/scheduler/scheduler.go` — leader-only `Scheduler` struct; subscribes to control-plane watches on startup; runs `reconcile()` on debounced events and on a 30s ticker. Pauses (releases all watches) when the local node loses raft leadership.
- `internal/scheduler/placement.go` — pure functions: `PlaceReplica(serviceSpec, eligibleHosts, replicaIndex) host` for spread/pack/hosts modes; `EligibleHosts(serviceSpec, nodes) []hostname`.
- `internal/scheduler/rollout.go` — `RolloutPlan` lifecycle: `Start`, `AdvanceStep`, `Complete`, `Abort`. Each transition is a raft Apply.
- `internal/scheduler/health.go` — observes `ReplicaObserved` health transitions; emits restart commands; tracks consecutive-restart count per replica id with reset-on-healthy.
- `internal/scheduler/drain.go` — graceful node remove orchestration: places replacements, waits for health, stops drained replicas, then issues `Cluster.NodeRemove` raft membership change.

Entity additions to control-plane state:

- `RolloutPlan {deployment, service, target_revision, total_steps, current_step, started_at, last_step_at, state ∈ {in_progress, completed, aborted}, failure_reason?}` — one per service undergoing an image/replica/placement change.
- `ReplicaCounter {deployment, service, next_index}` — singleton per service; raft Apply increments on new replica creation.
- `RestartCounter {replica_id, consecutive_failures, last_attempt_at}` — emerges as needed; deleted on first successful health check.

Placement rules (closed set per `ServiceSpec` shape from §5 of design):

- `hosts: [a, b]` mode: replicas placed only on hosts in the list; `eligible = hosts ∩ healthy_nodes`. If `len(eligible) < replicas`, scheduler emits `Status{code: pending, message: cannot satisfy host placement, details: {missing: [...]}` and places nothing.
- `placement: spread` (default): `eligible = all healthy nodes`; replica index `i` placed on `eligible[hash(deployment+service+i) mod len(eligible)]`.
- `placement: pack`: `eligible = all healthy nodes`; sort by current replica count (any service) ascending; place on the lowest-count host. Ties broken by hostname lexicographic order.

Rolling update step machine (per service, replicas: N, target image change):

- Step `k` of `N`: pick the kth replica (by index), write `ReplicaDesired{id: <dep>-<svc>-<k>, image: target}`, advance `RolloutPlan.current_step = k+1`. Wait for `ReplicaObserved{state: running, last_health_at recent}` on that replica before advancing.
- Invariant: at no step is more than 1 replica in the target service simultaneously not-running (excluding the one being replaced, which is stopped only when its replacement is healthy).
- Step timeout: 60s default per step before the rollout aborts (`RolloutPlan.state = aborted, failure_reason`). The previous successful revision continues to serve.

Health-driven actions (closed):

- `ReplicaObserved.state` transition to `degraded` (health check failing past compose threshold) → scheduler writes `ReplicaCommand{replica_id, op: remove_from_routing}` (consumed by ingress via Routes watch) + `ReplicaCommand{replica_id, op: restart}` (consumed by runtime via Replicas watch).
- After 3 consecutive `ReplicaObserved.state` transitions to `failed` without an intervening `running`, scheduler writes `ReplicaObserved{state: failed, code: restart_exhausted}` and does not issue further restarts. The replica is reconsidered only on next `Deploy.Apply`.

Drain sequence (graceful node remove):

- Operator runs `jaco node remove <hostname>`.
- Scheduler enumerates `ReplicaDesired` where `host = <hostname>`.
- For each: scheduler computes a new host using §4 placement on the remaining eligible set, writes `ReplicaDesired{id, host: new_host}`. Old replica on the leaving host remains `running` and routable until the new one passes health.
- When all replacements report `running`, scheduler writes `ReplicaDesired{id, host: removed}` for the old ones; runtime on the leaving node stops + removes those containers.
- Scheduler issues `Cluster.NodeRemove(<hostname>)` raft membership change; FSM removes the Node entity.
- Drain step timeout: 5 minutes per replica replacement; on timeout the node remove is aborted and `Status{pending: drain_timeout}` reported.

## §5 Sequence

Watch subscribe on leader election:

1. Local node becomes raft leader; `Scheduler.Start()` runs.
2. Subscribes to `Deployments`, `Nodes`, `ReplicaObserved` watches with `since_revision = last_applied`.
3. After catch-up phase completes, runs an initial `reconcile()` to fix any drift accumulated during the gap.
4. Enters event loop: on incoming watch event, debounce 50ms then `reconcile()`; on 30s tick, `reconcile()` unconditionally.

Reconcile pass:

1. Snapshot the current state (Deployments, Nodes-healthy, ReplicaDesired, ReplicaObserved) from in-memory typed stores.
2. For each Deployment.service:
   a. Compute desired replica set `D = {(id, host) for i in 0..replicas-1}` via placement.
   b. Diff against current `ReplicaDesired` for that service: adds, removes, host-moves, image-changes.
   c. If image change and `replicas > 1` and no active `RolloutPlan`: start a `RolloutPlan{total_steps: replicas, current_step: 0}` and execute step 0.
   d. If active `RolloutPlan`: check whether `current_step` replica reports `running` with recent health → advance to `current_step+1`; complete when `current_step == total_steps`.
3. Submit all required mutations as one batched raft Apply (multi-command batch) to keep apply-to-steady-state under 15s.

Apply path from CLI:

1. `Deploy.Apply` handler validates yaml, writes `Deployment{applied_revision: N+1, previous_revision: N}` via raft.
2. FSM applies; deployment watch fires.
3. Scheduler's event loop receives event, debounces, runs reconcile.
4. Reconcile computes desired replica set; emits diff as raft Applies.
5. Runtime sees ReplicaDesired changes via its watch (runtime slice).
6. Runtime reports `ReplicaObserved.state: running` after container starts and passes first health.
7. CLI's apply RPC returns success when all desired replicas observed running (subject to step timeouts), or returns the typed error from the rollout abort.

Pinned-host failure mode:

1. `ServiceSpec.hosts: [A]` and node A is unhealthy.
2. Reconcile computes `eligible = []`; placement returns empty.
3. Scheduler writes `Deployment.status = pending, details: {reason: cannot_satisfy_host_placement, host: A}` (visible in `jaco status`).
4. No `ReplicaDesired` writes; no replicas scheduled elsewhere.

## §6 Out of scope

- Specific docker engine integration (lives in runtime slice).
- Specific cert/route propagation to ingress (lives in ingress slice).
- DNS / WireGuard reactions to placement changes (lives in discovery slice).
- Cross-cluster replica placement (spec §3 Out: no federation).
- Auto-scaling (spec §3 Out).
- Resource quotas beyond compose `ulimits`/`tmpfs` (spec §3 Out).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
