Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 20, 18

# Task 21 — scheduler-reconcile

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Leader-only scheduler with watch-driven reconcile pass (50ms debounce + 30s safety tick) that emits desired-state mutations as a batched raft Apply.

## Tasks
- [ ] Create `internal/scheduler/scheduler.go` with `type Scheduler struct { ... }` and `Start(ctx)`. Constructor receives `raft *raft.Node`, `store *state.Store`, `brokers *watch.Registry`.
- [ ] Run only when local node is raft leader: subscribe to leader-state changes; on leader gain, open `brokers.Deployments().Subscribe`, `Nodes`, `ReplicaObserved` from `since_revision = lastAppliedIndex`. On leader loss, cancel all subscriptions; stop reconciling.
- [ ] Event loop: 50ms debounce on incoming watch events; 30s ticker. Both call `reconcile(ctx)`.
- [ ] `reconcile`: snapshot in-memory stores; for each Deployment.service compute desired set `D = {(id, host) for i in 0..replicas-1}` via `placement.PlaceReplica`; diff against current `ReplicaDesired`; collect mutations (adds, host-moves on placement change, image-changes, removes); submit as one `Command{Batch}{children: [...]}` raft Apply.
- [ ] Pinned-host failure: if placement returns `cannot_satisfy_host_placement`, raft-Apply `Command{DeploymentStatusUpdate}{deployment, status: pending, details}` without writing any ReplicaDesired.
- [ ] Add `Command{Batch}` variant to `proto/jaco/v1/commands.proto`; FSM `Apply` (task 04) routes by iterating children.
- [ ] Create `internal/scheduler/scheduler_test.go`: bootstrap raft+FSM+store; install scheduler; apply a 3-replica Deployment via raft directly; assert 3 ReplicaDesired created within 1s, evenly spread across 3 mock nodes.
- [ ] Create `scripts/test/scheduler-spread.sh` 3-node E2E asserting replicas placed on 3 distinct hosts.

## Acceptance criteria
- [ ] `go test ./internal/scheduler/... -race -count=1 -run Reconcile` exits 0.
- [ ] `bash scripts/test/scheduler-spread.sh` exits 0; output asserts replicas distributed across 3 hosts.
- [ ] Test asserts reconcile pause when leader lost (no further raft Applies after demotion).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
