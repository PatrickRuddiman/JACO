Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 22

# Task 23 — health-restart-and-drain

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
3-strike restart on health failure (`code: restart_exhausted` after) + drain-on-node-remove that places replacements before stopping evictees and tears down the raft member only afterwards.

## Tasks
- [ ] Create `internal/scheduler/health/health.go`: subscribe to `ReplicaObserved`. On `state → degraded`: raft-Apply `Command{ReplicaCommand}{replica_id, op: remove_from_routing}` (consumed by ingress via Routes watch — that wiring lives in ingress slice rebuild) AND `Command{ReplicaCommand}{replica_id, op: restart}` (consumed by runtime via Replicas watch).
- [ ] Track per-replica consecutive-failure count via `RestartCounter` entity: increment on each `state → failed` without intervening `state → running`. After 3 consecutive failures: raft-Apply `Command{ReplicaObservedUpdate}{state: failed, code: restart_exhausted}`; no further restart commands until next Deploy.Apply (which clears the RestartCounter).
- [ ] Create `internal/scheduler/drain/drain.go`: on `Cluster.NodeRemove(hostname, force=false)` request, enumerate `ReplicaDesired{host: hostname}`; compute replacements via `placement.PlaceReplica` against the eligible set minus the leaving node; raft-Apply `Command{Batch}{children: [ReplicaDesired updates with new host for each]}`.
- [ ] Old replicas on the leaving host remain `running` (routable via ingress) until each replacement reports `running`. Then raft-Apply `Command{ReplicaDesired}{host: removed}` so runtime on the old host tears them down. Then `raft.RemoveServer(hostname)` + `Command{NodeRemove}`.
- [ ] Drain timeout: 5min per replacement. On timeout abort drain: raft-Apply `Command{NodeStatusUpdate}{hostname, status: "drain_timeout"}`; old replicas remain.
- [ ] Tests: `internal/scheduler/health/health_test.go` — 3 successive failures → restart_exhausted, fourth failure does not emit a restart command. `internal/scheduler/drain/drain_test.go` — node with 2 replicas removed, both migrate, then raft membership removes the node.
- [ ] E2E `scripts/test/drain-node.sh` on 3-node rig.

## Acceptance criteria
- [ ] `go test ./internal/scheduler/health/... ./internal/scheduler/drain/... -race -count=1` exits 0.
- [ ] `bash scripts/test/drain-node.sh` exits 0.
- [ ] Test asserts no `ReplicaCommand{op:restart}` is written after the third failure.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
