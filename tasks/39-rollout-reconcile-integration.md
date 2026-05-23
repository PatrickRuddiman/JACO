Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 22, 38

# Task 39 — rollout-reconcile-integration

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Replace iter 29's minimal one-at-a-time image-change gate with the full `rollout.Rollout` state machine driven from `scheduler.Scheduler.Reconcile`. Image changes start a `RolloutPlan`, advance one step per healthy-replica observation, complete when all steps land, and abort + roll back on `step_timeout`.

## Tasks
- [ ] `internal/scheduler/scheduler.go:46`: add `rollouts *rollout.Rollout` to the `Scheduler` struct and a matching constructor arg in `scheduler.New`. Nil rollout falls back to iter 29's `isRollingImageChange` one-at-a-time gate so existing tests don't regress.
- [ ] `internal/scheduler/scheduler.go:113`: in `Reconcile`, before the per-deployment loop, call `rollouts.CheckTimeouts` so stale `IN_PROGRESS` plans abort + roll back.
- [ ] `internal/scheduler/scheduler.go:157` (`reconcileService`): replace the `isRollingImageChange` branch with `rolloutDriver` — when no plan exists and the current replicas have a different image, call `rollouts.Start(deployment, service, dep.Revision, int(svc.Replicas))`. When a plan is `IN_PROGRESS`, emit `ReplicaDesiredUpsert` only for `plan.CurrentStep`. When `StepReady(deployment, service)` returns true, call `AdvanceStep`. When `CurrentStep == TotalSteps`, call `Complete`.
- [ ] `internal/scheduler/scheduler.go`: remove `isRollingImageChange` once `rolloutDriver` replaces it; the test file's `TestReconcile_ImageChangeRollsOneAtATime` should remain green by depending on the new driver instead.
- [ ] `internal/daemon/grpc/server.go:225` (`startSubsystems`): construct `rollout.New(st, apply, rollout.SystemClock())` and pass it into `scheduler.New`.
- [ ] `internal/scheduler/scheduler_test.go`: add `TestReconcile_RolloutAbortsOnStepTimeout` — three replicas on `image:v1`, image bumped to `image:v2`, never report the v2 replica RUNNING; advance the Clock past `StepTimeout`; reconcile; assert the RolloutPlan transitioned to `ABORTED` and the deployment's `applied_revision` was rolled back via the `DeploymentRollback` batched with `Abort`.

## Acceptance criteria
- [ ] `go test ./internal/scheduler/... -race -count=1` exits 0.
- [ ] `git grep -nE 'rollouts\.Start' internal/scheduler/scheduler.go` matches.
- [ ] `git grep -nE 'rollouts\.CheckTimeouts' internal/scheduler/scheduler.go` matches.
- [ ] `git grep -nE 'isRollingImageChange' internal/scheduler/` returns 0 hits (helper deleted).
- [ ] `go test ./... -race -count=1` exits 0 across the whole tree.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
