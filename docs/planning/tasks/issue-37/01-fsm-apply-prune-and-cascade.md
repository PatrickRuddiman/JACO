Parent slice: [TCP ingress — control-plane](../../slices/issue-37/control-plane.md)
Depends on: 00

# Task 01 — FSM replace-set apply, delete cascade, snapshot

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Make `DeploymentApply` replace-set both HTTP `Route`s and `TCPRoute`s for the deployment (pruning entries dropped from a new revision), cascade `TCPRoute` removal on delete, and round-trip `TCPRoute`s through snapshots.

## Tasks
- [x] In `internal/controlplane/fsm/fsm.go` `DeploymentApply` (`fsm.go:144-162`), replace the upsert-only route loop (`fsm.go:160-162`) with replace-set semantics: first remove every `Route` in `f.State.Routes.List()` whose `GetDeployment() == da.GetDeployment()` (via `f.State.Routes.Remove(state.RouteKey(domain, path), idx)`), then `Routes.Apply` each `da.GetRoutes()`.
- [x] In the same `DeploymentApply` block, do the equivalent for TCP: remove every `TCPRoute` in `f.State.TCPRoutes.List()` whose `GetDeployment() == da.GetDeployment()` (via `f.State.TCPRoutes.Remove(state.TCPRouteKey(port), idx)`), then `TCPRoutes.Apply` each `da.GetTcpRoutes()`.
- [x] In `DeploymentDelete` (`fsm.go:187-208`), add a cascade loop removing every `TCPRoute` owned by the deployment, beside the existing `Routes` cascade (`fsm.go:189-193`).
- [x] In `internal/controlplane/fsm/snapshot.go`, include `f.State.TCPRoutes.List()` in the snapshot save (next to Routes, `snapshot.go:22`) and re-`Apply` each on restore (next to the Routes restore, `snapshot.go:70`).
- [x] Add FSM tests to `internal/controlplane/fsm/fsm_test.go`: (a) apply with two routes then re-apply with one drops the stale `Route` (HTTP prune); (b) same for `TCPRoute`; (c) `DeploymentDelete` removes the deployment's `TCPRoute`s; (d) snapshot save→restore round-trips `TCPRoute`s.

## Acceptance criteria
- [x] `go test ./internal/controlplane/fsm/ -run 'DeploymentApply|DeploymentDelete|Snapshot'` passes, including the prune cases for both HTTP and TCP.
- [x] `go build ./...` exits 0.
- [x] `git grep -nE 'TCPRoutes\.(Remove|Apply|List)' internal/controlplane/fsm/fsm.go internal/controlplane/fsm/snapshot.go` matches in both files.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
