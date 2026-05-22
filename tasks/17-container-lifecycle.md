Parent slice: [runtime](../slices/runtime.md)
Depends on: 16

# Task 17 — container-lifecycle

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Idempotent container `create-with-labels` / `start` / `stop` / `remove`; orphan reconcile that removes containers whose `jaco.replica_id` no longer maps to a `ReplicaDesired`.

## Tasks
- [ ] Create `internal/runtime/lifecycle/lifecycle.go` with `Start(ctx, spec ContainerSpec) error`, `Stop(ctx, replicaID string, gracePeriod time.Duration) error`, `Remove(ctx, replicaID string) error`, `Inspect(ctx, replicaID string) (containerID, state string, err error)`.
- [ ] Start: `ContainerList(All=true, Filters={"label":["jaco.replica_id=<id>"]})`. If exists and `jaco.raft_index == spec.RaftIndex`, return nil (idempotent). If exists with different `raft_index`, stop+remove first, then create.
- [ ] Create: apply every JACO label (`jaco.cluster_id`, `.deployment`, `.service`, `.replica_id`, `.replica_index`, `.raft_index`); `NetworkingConfig.EndpointsConfig = {}` (NetworkMode = "none" initially); after create, runtime caller invokes `NetworkConnect` per bridge from task 27.
- [ ] Create `internal/runtime/lifecycle/orphan.go` with `Reconcile(ctx, store *state.Store, d Docker) error`: list containers with `jaco.cluster_id=<this>`; for each whose `jaco.replica_id` is absent in `store.ReplicasDesired`, stop and remove.
- [ ] Wire orphan reconcile into `jaco serve` startup (runtime sub-init).
- [ ] Integration test `internal/runtime/lifecycle/lifecycle_test.go` (build tag `docker`, skipped automatically if `/var/run/docker.sock` is absent): launch busybox replica, assert running; stop+remove; orphan test path: manually create a JACO-labeled container, call `Reconcile` with an empty ReplicasDesired store, assert the container was removed.

## Acceptance criteria
- [ ] `go test -tags=docker ./internal/runtime/lifecycle/... -race -count=1` exits 0 when docker is available; skipped (still exit 0) otherwise.
- [ ] Test asserts orphan reconcile removed exactly the container with no matching ReplicaDesired entry.
- [ ] Test asserts re-running `Start` for an existing matching container is a no-op (no new container created).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
