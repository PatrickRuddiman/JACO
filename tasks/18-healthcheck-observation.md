Parent slice: [runtime](../slices/runtime.md)
Depends on: 17

# Task 18 — healthcheck-observation

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Per-replica health poll on the docker engine; translate to the closed `ReplicaState` set; write `ReplicaObserved` to raft via `Command{ReplicaObservedUpdate}`.

## Tasks
- [ ] Create `internal/runtime/health/health.go` defining `type Watcher struct { ... }` with `Start(ctx, replicaID, containerID string, hasHealthcheck bool)` and `Stop(replicaID string)`.
- [ ] Polling cadence: 1s while state != running; 5s while running. Use `ContainerInspect`.
- [ ] State mapping: docker `State.Health.Status` → `ReplicaState`: `starting → pending`, `healthy → running`, `unhealthy → degraded`. When `hasHealthcheck == false`: `State.Status == "running"` observed in 5 consecutive polls → running; `exited` → failed with `code: container_exited` and `details: {exit_code}`.
- [ ] On every state change emit `ReplicaObserved{state, started_at, container_id, last_health_at}` via control-plane RPC `Internal.Submit(Command{ReplicaObservedUpdate})`.
- [ ] Integration test `internal/runtime/health/health_test.go` (build tag `docker`): launch busybox with `HEALTHCHECK CMD-SHELL exit 0` via direct create; assert ReplicaObserved transitions `pending → running` within 10s, with `last_health_at` within 5s of now.
- [ ] Second test: launch busybox without healthcheck and a `sleep 1`; assert state transitions to `failed` with `code:container_exited` and `exit_code:0`.

## Acceptance criteria
- [ ] `go test -tags=docker ./internal/runtime/health/... -race -count=1` exits 0 (or skipped if docker absent).
- [ ] Test asserts `ReplicaObserved.last_health_at` is non-zero and within 10s of `time.Now()` once running.
- [ ] Test asserts the closed `ReplicaState` set is honored — no other state values appear in raft.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
