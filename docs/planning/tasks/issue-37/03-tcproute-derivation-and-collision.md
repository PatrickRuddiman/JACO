Parent slice: [TCP ingress — control-plane](../../slices/issue-37/control-plane.md)
Depends on: 00

# Task 03 — TCPRoute derivation + port_conflict collision check

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
At apply admission, derive `TCPRoute`s from the compose project's qualifying published ports, reject cluster-global port collisions with a structured `port_conflict`, and carry the derived routes in the `DeploymentApply` command.

## Tasks
- [x] Add `toTCPRoutes(deployment string, project *composetypes.Project) []*pb.TCPRoute` to `internal/controlplane/grpc/jaco_spec.go` (mirror `toRoutes` at `jaco_spec.go:175`). For each service and each `ServicePortConfig`, qualify an entry when `Published` parses to a positive int, `Protocol` is `"tcp"` or empty, and `HostIP` is empty or `"0.0.0.0"`; emit `&pb.TCPRoute{PublishedPort: published, Deployment: deployment, Service: svc.Name, ContainerPort: target}`. Skip all other entries (bare/ephemeral, UDP, host-IP-scoped).
- [x] In `internal/controlplane/grpc/deploy.go` `Apply` (`deploy.go:41`), after `parseAndValidate` (`deploy.go:49`) and before building the command (`deploy.go:91`): build the derived set via `toTCPRoutes`, then run the collision check.
- [x] Intra-apply collision: two qualifying entries in this compose (across services) sharing a `published_port` → `return nil, errorStatus(codes.InvalidArgument, "port_conflict", <msg naming both services + the port>)`.
- [x] Cross-deployment collision: any derived `published_port` already present in `d.state.TCPRoutes` whose stored `GetDeployment()` differs from `jacoSpec.Deployment` → `port_conflict` naming the conflicting deployment + the port. A re-apply of the **same** deployment reclaiming its own ports is not a conflict.
- [x] Pack `TcpRoutes: toTCPRoutes(jacoSpec.Deployment, composeProject)` into the `DeploymentApply` payload (`deploy.go:94-101`, beside `Routes`).
- [x] Add tests to `internal/controlplane/grpc/jaco_spec_test.go` (derivation) and `internal/controlplane/grpc/deploy_test.go` (collision).

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/ -run 'TCPRoute|ToTCPRoutes|PortConflict|Apply'` passes with: `"5432:5432"` → `TCPRoute{5432,...,5432}`; bare `"5432"`, `"5432:5432/udp"`, and `"127.0.0.1:5432:5432"` produce no `TCPRoute`; a second deployment publishing a port owned by another → `InvalidArgument`/`port_conflict`; re-applying the same deployment's own port → no conflict; two services in one compose publishing the same port → `port_conflict`.
- [x] `go build ./...` exits 0.
- [x] `git grep -nE 'toTCPRoutes|port_conflict' internal/controlplane/grpc/jaco_spec.go internal/controlplane/grpc/deploy.go` matches both files.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
