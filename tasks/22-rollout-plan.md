Parent slice: [scheduler](../slices/scheduler.md)
Depends on: 21

# Task 22 — rollout-plan

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`RolloutPlan` entity + step machine enforcing "never below `replicas-1` healthy" + 60s step timeout with abort to previous revision.

## Tasks
- [ ] Create `internal/scheduler/rollout/rollout.go` exposing `Start(deployment, service string, targetRev uint64, totalSteps int) error`, `AdvanceStep(deployment, service string, step int) error`, `Complete(deployment, service string) error`, `Abort(deployment, service, reason string) error`. Each transition is a `Command{RolloutPlanUpdate}` raft Apply.
- [ ] Step machine: scheduler.reconcile, on detecting image change with `replicas > 1` and no active RolloutPlan, calls `rollout.Start`. Step k writes `ReplicaDesired{id:<dep>-<svc>-<k>, image: target}`. Wait until `ReplicaObserved{id, state: running, last_health_at within 10s}` before `AdvanceStep(k+1)`. When `current_step == total_steps`, call `Complete`.
- [ ] Invariant enforcer: rollout.go computes "currently not-running in target service" before each step write; if value > 1 (excluding the replica being replaced), refuse to advance and emit `AuditEvent{type: ROLLOUT_INVARIANT_HOLD}` (add this to the closed AuditEventType set).
- [ ] Step timeout: 60s per step. Tracked by `RolloutPlan.last_step_at`. On timeout: `rollout.Abort(reason: "step_timeout")` — restore previous `Deployment.applied_revision`, re-derive ReplicaDesired from it. Audit event `ROLLBACK` (auto-triggered).
- [ ] Integration test in `internal/scheduler/rollout/rollout_test.go`: 3-replica deployment v1; apply v2; assert rollout completes with `RolloutPlan.state == "completed"`; during rollout, poll the FSM `state.ReplicasObserved`, assert at no observation more than 1 replica is not-running.
- [ ] Second test: apply v3 that always health-fails (mocked); assert `RolloutPlan.state == "aborted"` within 60s+jitter and v2 image is still served by all 3 replicas.

## Acceptance criteria
- [ ] `go test ./internal/scheduler/rollout/... -race -count=1` exits 0.
- [ ] Test asserts the `replicas-1` invariant across the entire rollout window (no observation violates).
- [ ] Test asserts abort restores previous revision in raft state.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
