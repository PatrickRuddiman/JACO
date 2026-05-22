Parent slice: [runtime](../slices/runtime.md)
Depends on: 16

# Task 17 — container-lifecycle

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Idempotent container `create-with-labels` / `start` / `stop` / `remove`; orphan reconcile that removes containers whose `jaco.replica_id` no longer maps to a `ReplicaDesired`.

## Tasks
- [x] Create `internal/runtime/lifecycle/lifecycle.go` with `Start(ctx, d, spec) (containerID, error)`, `Stop(ctx, d, replicaID, gracePeriodSeconds) error`, `Remove(ctx, d, replicaID) error`, `Inspect(ctx, d, replicaID) (containerID, state, error)`. All four take the narrow `dockerx.Docker` interface so pure-Go tests use an in-memory fake.
- [x] Start lists containers by `jaco.replica_id=<id>` label. If exists with matching `jaco.raft_index`, no-op (returns the existing id). If exists with different raft_index, stop+remove then create. Container name `jaco_<replica_id>`.
- [x] Container create wires every JACO label (cluster_id, deployment, service, replica_id, replica_index, raft_index) from the ContainerSpec. NetworkMode forced to `none` — task 27 attaches bridges via NetworkConnect post-create.
- [x] `internal/runtime/lifecycle/config.go` projects compose.ContainerSpec into (*container.Config, *container.HostConfig, *network.NetworkingConfig) covering env / healthcheck / cap_add+drop / sysctls / read_only / tmpfs / mounts / ulimits (via HostConfig.Resources.Ulimits in moby v28).
- [x] `internal/runtime/lifecycle/orphan.go` exposes `Reconcile(ctx, d, clusterID, expectedReplicaIDs)`: lists containers with `jaco.cluster_id=<id>`, stop+removes any whose `jaco.replica_id` is absent from expectedReplicaIDs, returns the removed replica ids for audit. Other clusters' containers are left untouched.
- [ ] Wiring orphan reconcile into `jaco serve` startup is **deferred** to task 17's followup-pair: the daemon entrypoint itself, which currently doesn't exist as a self-contained component. The Reconcile function here is the building block; the daemon will call it from runtime sub-init when it lands.
- [x] Twelve pure-Go tests pass with -race (no docker socket required): Start writes all six JACO labels + name; Start is a no-op when (replica_id, raft_index) matches; Start stop+remove+recreates when raft_index changed; Stop no-op on missing + transitions running→exited; Remove deletes + no-op on missing; Inspect returns id+state + empty when missing; Reconcile removes orphans + leaves other clusters' containers alone; Reconcile requires clusterID; Start requires ReplicaID + Image.
- [ ] Real-engine integration test (build tag `docker`) is **deferred** — the in-memory fake covers the contract (idempotence, orphan cleanup, label propagation) the integration test would assert. Real-engine assertions land alongside the discovery slice's CI test rig in task 31.

## Acceptance criteria
- [x] `go test ./internal/runtime/lifecycle/... -race -count=1` exits 0 (12 pure-Go tests pass).
- [x] Test asserts orphan reconcile removes exactly the orphan replica_id (`TestReconcile_RemovesOrphans`).
- [x] Test asserts re-running `Start` for a matching (replica_id, raft_index) is a no-op (`TestStart_IsNoopWhenReplicaAlreadyMatchesRaftIndex`).
- [ ] `go test -tags=docker ./internal/runtime/lifecycle/... -race -count=1` against a real docker engine — deferred to task 31's CI test rig.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
