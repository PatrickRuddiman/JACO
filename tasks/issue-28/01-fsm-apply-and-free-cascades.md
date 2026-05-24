Parent slice: [control-plane](../../slices/issue-28/control-plane.md)
Depends on: 00

# Task 01 — fsm-apply-and-free-cascades

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Free subnets correctly — by most-specific key — and cascade subnet frees when a deployment is deleted or a node is removed (closing the existing leak).

## Tasks
- [x] `internal/controlplane/fsm/fsm.go:345` — `SubnetFree` apply: remove by `(deployment, network, host)` when all three are set; when `host` and/or `network` are empty, iterate `f.State.Subnets.List()` and `Remove` every entry matching the set fields.
- [x] `internal/controlplane/fsm/fsm.go:180` — in the `DeploymentDelete` cascade, add a pass removing every `Subnet` whose `GetDeployment() == name`.
- [x] `internal/controlplane/fsm/fsm.go:104` — in the `NodeRemove` cascade, add a pass removing every `Subnet` whose `GetHost() == hostname`.

## Acceptance criteria
- [x] `go test ./internal/controlplane/fsm/ -race -count=1` passes (unit: SubnetFree by full key removes one; SubnetFree with empty host removes all hosts of a `(dep,net)`; DeploymentDelete frees every subnet of the deployment; NodeRemove frees every subnet on the host; unrelated subnets untouched).
- [x] `go build ./...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
