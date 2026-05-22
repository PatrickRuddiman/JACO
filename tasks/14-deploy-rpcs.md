Parent slice: [control-plane](../slices/control-plane.md), [scheduler](../slices/scheduler.md)
Depends on: 04, 13

# Task 14 — deploy-rpcs

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`Deploy.Apply` (with `--dry-run` Diff), `Deploy.Rollback`, and `Deploy.Delete` server handlers; Deployment + Route entity writes through raft.

## Tasks
- [x] Add `Deploy.Apply(ApplyRequest{jaco_yaml, compose_yaml, dry_run}) returns (ApplyResponse{applied_revision, diff})` handler in `internal/controlplane/grpc/deploy.go`. Operator-authenticated; requires leader; falls through to `errorStatus(no_leader)` if no leader is known.
- [x] Define the jaco.yaml schema in `internal/controlplane/grpc/jaco_spec.go`: `deployment`, `services[{name, replicas, placement: spread|pack|hosts, hosts, compose_service, networks}]`, `routes[{domain, service, port, tls: auto|off}]`. `ParseJacoYAML` applies defaults (placement spread, tls auto, compose_service defaults to service name). `validateJacoYAML` enforces invariants (non-empty deployment, valid placement, hosts required for placement=hosts, no duplicate service names, routes reference declared services, port > 0).
- [x] On Apply: run `compose.Validate` + `compose.LoadBytes`, then assert each `JacoServiceDecl.compose_service` exists in the parsed compose project. Any failure returns `Error{code:"validation_failed", message}` (or `"unknown_network"` from compose.Validate).
- [x] On `dry_run`: return `Diff{}` + `applied_revision = current_revision` (unchanged). No raft.Apply.
- [x] On non-dry-run: build `Command{DeploymentApply}` with the parsed services + routes + raw YAMLs and raft-Apply. Return the new `applied_revision`.
- [x] `computeDiff` returns an empty Diff in v1 — the scheduler (task 21) materializes ReplicaDesired from `Deployment.services`, and IPAM (task 25) allocates Subnets; once those land, the Apply path can populate adds/updates/removes here.
- [x] Add `Deploy.Rollback(req{deployment})` looking up `Deployment.previous_revision`; raft-Applies `Command{DeploymentRollback}`. FSM flips applied/previous revision markers and audits ROLLBACK. Returns `FailedPrecondition / no_previous_revision` when no previous revision exists. Full state restore (re-deriving services/routes from prior YAML) requires revision history — a follow-up.
- [x] Add `Deploy.Delete(req{deployment})` which raft-Applies `Command{DeploymentDelete}` — the FSM (task 04) already cascades to remove Routes + ReplicaDesired for the deployment.
- [x] Register the `Deploy` service in `grpcsrv.NewServer` alongside Cluster / Tokens / Audit.
- [x] Create `internal/controlplane/grpc/deploy_test.go` covering: dry-run leaves state untouched; apply writes Deployment + 2 Routes with correct tls flags; second apply bumps applied_revision to 2 and previous_revision to 1; rollback flips them back; rollback refuses when there's no previous revision; delete cascades the Routes; apply rejects an unknown `compose_service` reference; apply rejects an unknown compose field (passes through `compose.Validate`'s `validation_failed`).

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/... -race -count=1 -run Deploy` exits 0.
- [x] Test asserts `dry_run` leaves the Deployment store unchanged.
- [x] Test asserts `Delete` removes all Routes belonging to the deployment.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
