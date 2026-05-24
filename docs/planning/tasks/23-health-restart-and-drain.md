Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 22

# Task 23 — health-restart-and-drain

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
3-strike restart on health failure (`code: restart_exhausted` after) + drain-on-node-remove that places replacements before stopping evictees and tears down the raft member only afterwards.

## Tasks
- [x] Add `Command{RestartCounterUpdate}` variant to commands.proto (ACTION_INCREMENT / ACTION_RESET) plus the FSM handler in `internal/controlplane/fsm/fsm.go` so the scheduler/health package can bump or reset the RestartCounter entity.
- [x] Create `internal/scheduler/health/health.go` Restarter. Handles ReplicasObserved events on the raft leader: DEGRADED → batched `ReplicaCommand{remove_from_routing}` + `ReplicaCommand{restart}`; FAILED → bump RestartCounter + emit ReplicaCommand{restart}; on the 3rd consecutive failure with no intervening RUNNING, write `ReplicaObservedUpdate{FAILED, code:"restart_exhausted"}` and stop restarting. RUNNING resets the counter (idempotent). Filters out its own `restart_exhausted` echo so the loop terminates.
- [x] Run(ctx) subscribes to ReplicasObserved and drives Handle() for each event; Handle() self-gates on `leader.IsLeader()` so follower-side restarts never fire.
- [x] Create `internal/scheduler/drain/drain.go` with `Plan(state, hostname) ([]Migration, error)`: computes the per-replica migration plan for draining a host. Uses placement.PlaceReplica against the remaining eligible set (sans the draining host); rejects when no eligible host remains for a service (e.g. HOSTS-pinned to the draining host).
- [ ] **Deferred**: full drain step machine (await each replacement healthy → stop evictees → raft.RemoveServer) — needs the daemon entry to wire drain into Cluster.NodeRemove(force=false). Plan() is the building block.
- [ ] **Deferred**: 5min step timeout + NodeStatusUpdate{drain_timeout} — lands with the step machine.
- [x] Six restarter tests pass with -race: first failure increments counter + issues restart; three consecutive failures → restart_exhausted with no fourth restart (the AC); RUNNING resets the counter and the next failure starts again at 1; DEGRADED emits remove_from_routing + restart; follower no-ops; own restart_exhausted echo is filtered.
- [x] Five drain.Plan tests pass with -race: empty host has no migrations; spread-mode 3 replicas where one host drains → migration off; PACK with all replicas on draining host → all migrate to different hosts; HOSTS pinned to draining host → error; empty hostname rejected.
- [ ] **Deferred**: `scripts/test/drain-node.sh` E2E — depends on `jaco serve` daemon entry to drive Cluster.NodeRemove through the full step machine.

## Acceptance criteria
- [x] `go test ./internal/scheduler/health/... ./internal/scheduler/drain/... -race -count=1` exits 0 (11 tests).
- [x] Test asserts no `ReplicaCommand{op:restart}` is written after the third failure (`TestHandle_NoRestartAfterThreeConsecutiveFailures`).
- [ ] `bash scripts/test/drain-node.sh` — deferred to daemon entry.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
