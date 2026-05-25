Parent slice: [TCP ingress — control-plane](../../slices/issue-37/control-plane.md)
Depends on: none

# Task 00 — TCPRoute entity plumbing (proto + state store + watch broker)

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Materialize a `TCPRoute` entity end-to-end — proto message, replicated state store, and watch broker/fan-out — so later tasks have something to write, prune, and subscribe to. No producers or consumers yet.

## Tasks
- [ ] Add a `TCPRoute` message to `proto/jaco/v1/entities.proto` (after `Route` at `entities.proto:140`): `int32 published_port = 1; string deployment = 2; string service = 3; int32 container_port = 4;`.
- [ ] In `proto/jaco/v1/commands.proto`, add `repeated TCPRoute tcp_routes = 7;` to `DeploymentApply` (`commands.proto:110-117`, after `routes = 6`).
- [ ] In `proto/jaco/v1/fsm.proto`, add `repeated TCPRoute tcp_routes = 16;` to the `FSMSnapshot` message (`fsm.proto:12`, after `audit_events = 15`).
- [ ] In `proto/jaco/v1/events.proto`, add a `TCPRouteEvent` message mirroring `RouteEvent` (`events.proto:49`): `EventKind kind = 1; TCPRoute before = 2; TCPRoute after = 3; uint64 raft_index = 4;`.
- [ ] In `proto/jaco/v1/services.proto`, add `TCPRouteEvent tcp_route = 15;` to the `SubscribeEvent` oneof (`services.proto:236`, after `restart_counter = 14`).
- [ ] Run `make proto` (`buf generate`) to regenerate `pkg/proto/jaco/v1/*.pb.go`.
- [ ] Create `internal/controlplane/state/tcproutes.go`: `TCPRouteKey(publishedPort int32) string` returning the decimal port string, and `newTCPRoutes(b *watch.Broker[*pb.TCPRoute]) *Store[*pb.TCPRoute]` keyed on `r.GetPublishedPort()` (mirror `internal/controlplane/state/routes.go`).
- [ ] Add `TCPRoutes *Store[*pb.TCPRoute]` to `state.State` (`internal/controlplane/state/state.go:19`, next to `Routes`) and construct it as `newTCPRoutes(brokers.TCPRoutes)` in `New` (`state.go:40`).
- [ ] Add `TCPRoutes *Broker[*pb.TCPRoute]` to `watch.Registry` (`internal/controlplane/watch/registry.go:14`, next to `Routes`) and size it `NewBroker[*pb.TCPRoute](DefaultBuffer)` in `NewRegistry` (`registry.go:29`).
- [ ] Add `forwardTCPRoutes` to `internal/controlplane/grpc/watch.go` mirroring `forwardRoutes` (`watch.go:122`) — same `depFilter` behavior, emitting `&pb.SubscribeEvent{Payload: &pb.SubscribeEvent_TcpRoute{...}}` — and subscribe it in the Subscribe RPC alongside `forwardRoutes` (`watch.go:47-49`).
- [ ] Add `TCPRoute` store unit tests to `internal/controlplane/state/state_test.go` (key formatting; Apply/Get/List/Remove round-trip; broker publishes on Apply/Remove).

## Acceptance criteria
- [ ] `make proto` exits 0 and `git grep -nE 'type TCPRoute struct' pkg/proto/jaco/v1/entities.pb.go` matches.
- [ ] `git grep -nE 'TcpRoutes' pkg/proto/jaco/v1/commands.pb.go` matches (DeploymentApply + Snapshot getters generated).
- [ ] `go test ./internal/controlplane/state/ -run TCPRoute` passes.
- [ ] `go test ./internal/controlplane/watch/... ./internal/controlplane/grpc/ -run 'Broker|Watch|Subscribe'` passes.
- [ ] `go build ./...` exits 0.
- [ ] `git grep -nE 'TCPRoutes ' internal/controlplane/state/state.go internal/controlplane/watch/registry.go` matches both files.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
