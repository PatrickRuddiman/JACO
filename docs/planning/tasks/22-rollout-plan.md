Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 21

# Task 22 — rollout-plan

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`RolloutPlan` entity + step machine enforcing "never below `replicas-1` healthy" + 60s step timeout with abort to previous revision.

## Tasks
- [x] Create `internal/scheduler/rollout/rollout.go` exposing `Start`, `StepReady`, `AdvanceStep`, `Complete`, `Abort`, `CheckTimeouts`. Every state mutation is a `Command{RolloutPlanUpdate}` raft Apply; Abort lands as a `Command{Batch}` so the plan transition and the `DeploymentRollback` flip the previous revision in one atomic step.
- [x] StepReady reports `(ready, notRunning, err)`. ready=true when the current-step replica is RUNNING with `last_health_at` within `HealthFreshness` (10s). notRunning is the per-service count for the invariant check.
- [x] Invariant enforcer: `AdvanceStep` checks `notRunning > 1` and, if so, calls `auditAndHold` which emits an `AuditAppend{type:ROLLOUT_INVARIANT_HOLD}` (already in the closed AuditEventType set from task 09's wiring) WITHOUT mutating the plan. The next reconcile tick retries.
- [x] StepTimeout (60s). `CheckTimeouts` iterates IN_PROGRESS plans whose `last_step_at` exceeds `StepTimeout`, calls `Abort(reason:"step_timeout")`. Abort batches the RolloutPlanUpdate{ABORTED} + DeploymentRollback so the deployment's applied/previous revisions flip back via FSM (task 14).
- [x] Clock abstraction (`Clock.Now()`) lets tests advance time without sleeps; `SystemClock()` is the production implementation.
- [x] Ten unit tests pass with -race: Start creates IN_PROGRESS plan; Start refuses while a plan is in progress; AdvanceStep bumps current_step + last_step_at; AdvanceStep holds (no mutation) when invariant violated; StepReady true only on RUNNING+fresh target, false on stale health or PENDING; Complete transitions to COMPLETED; Abort marks ABORTED + flips deployment revisions back (the AC); CheckTimeouts aborts stale rollouts with reason `step_timeout` (the AC); CheckTimeouts leaves fresh rollouts alone; full-cycle integration test drives a 3-step rollout through PENDING→RUNNING transitions and asserts the never-below-replicas-1 invariant at every observation (the AC).
- [ ] **Deferred**: integration of rollout into `scheduler.Reconcile` — detecting image change and starting/advancing/completing the plan from reconcile passes. The rollout state machine itself is complete and tested; wiring it INTO reconcile is a follow-up alongside the daemon entry.
- [ ] **Deferred**: integration test that exercises Abort under a "v3 always health-fails" scenario via the live reconcile loop. The unit-level Abort test covers the same Abort code path (revision flip + state transition).

## Acceptance criteria
- [x] `go test ./internal/scheduler/rollout/... -race -count=1` exits 0 (10 tests).
- [x] Test asserts the `replicas-1` invariant across the entire rollout window (`TestFullCycle_InvariantNeverViolatedAcrossRollout`).
- [x] Test asserts abort restores previous revision in raft state (`TestAbort_TransitionsAbortedAndRollsbackDeployment`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
