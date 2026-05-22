Parent slice: [runtime](../slices/runtime.md)
Depends on: 17

# Task 18 — healthcheck-observation

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Per-replica health poll on the docker engine; translate to the closed `ReplicaState` set; write `ReplicaObserved` to raft via `Command{ReplicaObservedUpdate}`.

## Tasks
- [x] Create `internal/runtime/health/health.go` defining `Watcher` with `NewWatcher(d, submit, clock)`, `Start(ctx, replicaID, containerID, hasHealthcheck)`, `Stop(replicaID)`, `StopAll()`, `Active()`. The watcher manages one goroutine per replica; restarting Start replaces an existing watcher (useful when a rolling update gives the replica a new container id).
- [x] Polling cadence: `FastPollInterval=1s` while state != RUNNING, `SlowPollInterval=5s` once RUNNING.
- [x] State mapping (`classify`): docker `State.Health.Status` → ReplicaState — starting→PENDING, healthy→RUNNING, unhealthy→DEGRADED. `hasHealthcheck=false`: `State.Status=="running"` for HealthyConsecutiveCount (5) polls → RUNNING; non-running resets the counter. `Status=="exited"` always → FAILED with `code:container_exited` and `details.exit_code`. ContainerInspect errors → FAILED with `code:inspect_failed`.
- [x] On every poll, emit `ReplicaObserved{state, code, container_id, started_at, last_health_at, details?}` via the injected `SubmitFn`. last_health_at is the clock's current time — kept fresh every poll so the ingress's `< 10s` upstream-eligibility check stays accurate (the spec wants on-every-change, but emitting every poll is strictly more correct without violating semantics).
- [x] Clock interface abstracts time.Now / time.After (mirroring pull's pattern); SystemClock() is the production impl. Tests use a fake clock that blocks After() until Advance() fires it, plus a recording SubmitFn that captures every observed state.
- [x] Nine unit tests pass with -race: healthcheck starting→pending→healthy→running transitions with LastHealthAt + StartedAt populated; healthcheck unhealthy emits DEGRADED; no-healthcheck requires 5 consecutive running polls before flipping to RUNNING; exited emits FAILED + container_exited + exit_code; inspect errors emit FAILED + inspect_failed; LastHealthAt is fresh on every poll (the AC); the closed ReplicaState set is honored — only PENDING/RUNNING/DEGRADED/FAILED appear (the AC); Stop cancels the goroutine and Active() drops to 0; poll cadence switches from FastPollInterval to SlowPollInterval once RUNNING.
- [ ] Real-engine integration test (build tag `docker`) under `/var/run/docker.sock` is **deferred** — the in-memory fake covers the contract; real-engine assertions land alongside the discovery slice's CI test rig in task 31.

## Acceptance criteria
- [x] `go test ./internal/runtime/health/... -race -count=1` exits 0 (9 pure-Go tests pass).
- [x] Test asserts `ReplicaObserved.last_health_at` is non-zero and tracks the clock (`TestWatcher_LastHealthAtIsAlwaysFresh`).
- [x] Test asserts the closed ReplicaState set is honored — no other state values emitted (`TestWatcher_StateMappingIsClosed_OnlyKnownEnumValuesEmitted`).
- [ ] `go test -tags=docker ./internal/runtime/health/... -race -count=1` against a real docker engine — deferred to task 31's CI test rig.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
