Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 20, 18

# Task 21 — scheduler-reconcile

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Leader-only scheduler with watch-driven reconcile pass (50ms debounce + 30s safety tick) that emits desired-state mutations as a batched raft Apply.

## Tasks
- [x] Create `internal/scheduler/scheduler.go` with `Scheduler` struct + constructor taking `*state.State`, `*watch.Registry`, a `LeaderStatus` interface (raftnode.Node satisfies it), and an `Applier` (raft.Apply wrapper). `Run(ctx)` subscribes to Deployments / Nodes / ReplicasObserved and drives the reconcile loop until ctx cancellation.
- [x] Reconcile loop: 50ms debounce window on incoming watch events (any of three streams), 30s safety ticker that always fires Reconcile. Both call `s.Reconcile(ctx)` which itself is a no-op when `leader.IsLeader()` returns false (so the loop self-gates on follower nodes — the daemon entry can call Run unconditionally).
- [x] `Reconcile`: snapshot Deployments + Nodes; for each Deployment parse compose.yaml once; for each service compute the desired ReplicaDesired set via `placement.PlaceReplica`; diff against the per-service current set in `state.ReplicasDesired`. Collect adds + updates (host-moves on placement change, image-changes) + removes; raft-Apply everything as a single `Command{Batch}` so one reconcile pass lands atomically.
- [x] Pinned-host failure: `placement.PlaceReplica` returning `cannot_satisfy_host_placement` short-circuits this service's reconcile and emits `Command{DeploymentStatusUpdate}{status:PENDING, details.reason}` instead. No ReplicaDesired written for the service.
- [x] Unknown-compose-service failure (service.compose_service not found in compose.yaml): same pending-status short-circuit.
- [x] Compose parse failure (malformed Deployment.compose_yaml): same pending-status short-circuit.
- [x] `Command{Batch}` already exists in commands.proto + FSM (tasks 01 + 04); reconcile just nests its per-service commands inside.
- [x] Six unit tests pass with -race: a 3-replica deployment evenly spreads across 3 nodes (the primary AC — exactly 1 replica per node); leader-loss noop (the AC); reconcile is idempotent when current already matches desired; scale-down (3→1) removes the excess replicas via Command{ReplicaDesiredRemove}; pinned-host placement failure marks the deployment PENDING and writes no replicas; unknown compose_service marks the deployment PENDING.
- [ ] **Deferred**: 3-node `scripts/test/scheduler-spread.sh` E2E — depends on `jaco serve` which doesn't exist yet. The in-process integration test exercises the same end-to-end shape via the real state + FSM + brokers (no docker engine needed for the desired-state assertions).

## Acceptance criteria
- [x] `go test ./internal/scheduler/... -race -count=1 -run Reconcile` exits 0 (6 tests).
- [x] Test asserts the 3-replica spread (`TestReconcile_ThreeReplicaDeploymentEvenlySpreadAcrossThreeNodes`).
- [x] Test asserts reconcile pause when leader lost (`TestReconcile_NoopOnLeaderLoss`).
- [ ] `bash scripts/test/scheduler-spread.sh` — deferred to task 17's daemon entry.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
