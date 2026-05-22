Parent slice: [control-plane](../slices/control-plane.md), [scheduler](../slices/scheduler.md)
Depends on: 04, 13

# Task 14 — deploy-rpcs

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`Deploy.Apply` (with `--dry-run` Diff), `Deploy.Rollback`, and `Deploy.Delete` server handlers; Deployment + Route entity writes through raft.

## Tasks
- [ ] Add `Deploy.Apply(ApplyRequest{jaco_yaml_bytes, compose_yaml_bytes, dry_run}) returns (ApplyResponse{applied_revision, diff})` handler in `internal/controlplane/grpc/deploy.go`.
- [ ] Parse jaco.yaml (define schema in this task — minimal: `deployment: <name>; services: [{name, replicas, placement: spread|pack, hosts: [], compose_service: <name>, networks: []}]; routes: [{domain, service, port, tls: auto|off}]`). Call `compose.Load+Validate`. On either failing → return `Error{code:"validation_failed", details}`.
- [ ] Build current vs target replica sets via placement engine stubbed for this task (delegated to task 20 for the real impl; here return an empty diff if scheduler isn't wired yet). Build `Diff{adds[], updates[], removes[]}` of ReplicaDesired, Route, and Subnet entries.
- [ ] On `dry_run`: return Diff and `applied_revision = current_revision` without raft.Apply.
- [ ] Otherwise: build `Command{DeploymentApply}` proto, leader-forward via task-06 `EnsureLeader`, `raft.Apply` with 5s timeout; on success return new `applied_revision`.
- [ ] Add `Deploy.Rollback(req{deployment}) returns (RollbackResponse{revision})`: look up `Deployment.previous_revision`; raft-Apply `Command{DeploymentRollback}{deployment, revision}` which restores the prior `applied_revision` and re-derives ReplicaDesired/Routes.
- [ ] Add `Deploy.Delete(req{deployment}) returns (DeleteResponse{})`: raft-Apply `Command{DeploymentDelete}{deployment}` which cascades — removes Deployment + all Routes owned by it + all ReplicaDesired (runtime will tear containers down via watch).
- [ ] Create `internal/controlplane/grpc/deploy_test.go`: bootstrap; Apply a 1-service deployment with `dry_run=true` and assert no state change; without `dry_run` assert Deployment + 1 ReplicaDesired present; Rollback to previous revision; Delete and assert cascade.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/grpc/... -race -count=1 -run Deploy` exits 0.
- [ ] Test asserts `dry_run` leaves the Deployment store unchanged.
- [ ] Test asserts `Delete` removes all Routes belonging to the deployment.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
