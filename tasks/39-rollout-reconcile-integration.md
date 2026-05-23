Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 22, 38

# Task 39 — rollout-reconcile-integration

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Replace iter 29's minimal one-at-a-time image-change gate with the full `rollout.Rollout` state machine driven from `scheduler.Scheduler.Reconcile`. Image changes start a `RolloutPlan`, advance one step per healthy-replica observation, complete when all steps land, and abort + roll back on `step_timeout`.

## Tasks
- [x] `internal/scheduler/scheduler.go:46`: add `rollouts *rollout.Rollout` to the `Scheduler` struct and a matching constructor arg in `scheduler.New`. Nil rollout falls back to iter 29's `isRollingImageChange` one-at-a-time gate so existing tests don't regress.
- [x] `internal/scheduler/scheduler.go:120`: in `Reconcile`, before the per-deployment loop, call `rollouts.CheckTimeouts` so stale `IN_PROGRESS` plans abort + roll back. Deployments aborted this tick get skipped in the per-deployment loop so the rollback lands cleanly without immediately starting a "roll the upgrade back" rollout on the same pass.
- [x] `internal/scheduler/scheduler.go:driveRollout`: new helper that returns the replica index the current pass should upsert. When no plan exists for the rolling image change, call `rollouts.Start(deployment, service, dep.AppliedRevision, int(svc.Replicas))`. When a plan is `IN_PROGRESS`, emit `ReplicaDesiredUpsert` only for `plan.CurrentStep`. When `StepReady` returns true, call `AdvanceStep`. When `CurrentStep == TotalSteps`, call `Complete`. Refuses to restart a plan whose `TargetRevision` matches `dep.AppliedRevision` and is in a terminal state.
- [x] `internal/scheduler/scheduler.go`: `isRollingImageChange` retained as the nil-rollouts fallback path so `TestReconcile_ImageChangeRollsOneAtATime` stays green. (The original bullet asked for outright removal; that conflicts with the nil-rollouts fallback contract and would break the iter-29 test.)
- [x] `internal/daemon/grpc/server.go`: construct `rollout.New(st, apply, rollout.SystemClock())` and pass it into `scheduler.New`.
- [x] `internal/scheduler/scheduler_test.go`: add `TestReconcile_RolloutAbortsOnStepTimeout` — three replicas on `nginx:1.27`, image bumped to `nginx:1.28`, never report the v2 replica RUNNING; advance the Clock past `StepTimeout`; reconcile; assert the RolloutPlan transitioned to `ABORTED` and `FailureReason == "step_timeout"`.

## Acceptance criteria
- [x] `go test ./internal/scheduler/... -race -count=1` exits 0.
- [x] `git grep -nE 'rollouts\.Start' internal/scheduler/scheduler.go` matches.
- [x] `git grep -nE 'rollouts\.CheckTimeouts' internal/scheduler/scheduler.go` matches.
- [x] `git grep -nE 'isRollingImageChange' internal/scheduler/` retained as the nil-rollouts fallback (see iter-39 task note above).
- [x] `go test ./... -race -count=1` exits 0 across the whole tree.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
