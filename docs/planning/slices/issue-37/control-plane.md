Parent spec: [Issue #37 — ingress: add TCP port-forwarding and reject compose-level 80/443 binds](https://github.com/PatrickRuddiman/jaco/issues/37) (driving spec) · repo [spec.md](../../spec.md)

# TCP ingress — control-plane

## §1 Summary

The state model for TCP ingress: a new `TCPRoute` entity derived at admission from each deployment's compose `ports:`, written/pruned through the existing `DeploymentApply` FSM path, and removed on deployment delete. Owns the published-port collision check and the compose `reserved_port` (80/443) validation. Does **not** open listeners, forward packets, load-balance, or drain connections — that is the datapath slice.

## §2 Codebase reconnaissance

- HTTP `Route` entity at `proto/jaco/v1/entities.proto:140` — `{domain, deployment, service, port, tls_auto, path}`. Keyed `(domain, path)` via `state.RouteKey` (`internal/controlplane/state/routes.go:13`).
- Route derivation at admission: `toRoutes(deployment, decls)` (`internal/controlplane/grpc/jaco_spec.go:175`) builds `[]*pb.Route` from parsed jaco.yaml; the duplicate `(domain, path)` reject lives in `validateJacoYAML` (`jaco_spec.go:149-153`).
- Apply path: `deployForServer.Apply` (`internal/controlplane/grpc/deploy.go:41`) calls `parseAndValidate` (`deploy.go:193`) — runs `compose.Validate` (raw bytes) then `compose.LoadBytes` (typed `*composetypes.Project`, port ranges pre-expanded) — then packs `Routes: toRoutes(...)` into a `DeploymentApply` command (`deploy.go:100`).
- FSM `DeploymentApply` (`internal/controlplane/fsm/fsm.go:144-162`) writes the Deployment then **upsert-only** applies routes (`fsm.go:160`: `for _, r := range da.GetRoutes() { Routes.Apply(r, idx) }`) — it never prunes routes dropped from a new revision. `DeploymentDelete` (`fsm.go:187-208`) cascades: removes Routes, ReplicaDesired, and per-host Subnets by deployment.
- Compose validation: `compose.Validate(rawYAML)` (`internal/runtime/compose/validate.go:73`) hand-parses raw YAML for the closed field-set and `networks:`; returns typed `compose.ValidationError{Code, Message, Details}` (`internal/runtime/compose/types.go:123`). `ports` is in `allowedServiceFields` (`validate.go:20`) but its content is never inspected.
- Compose ports already parse into `PortDecl{Container, Host, Protocol}` (`internal/runtime/compose/types.go:94`, populated by `portsFromCompose`, `internal/runtime/compose/spec.go:286`) off compose-go's `types.ServicePortConfig{Published, Target, Protocol, HostIP}`. JACO never host-publishes these (`types.go:73` comment).
- Entity plumbing template (what a new entity touches): `Store[*pb.Route]` in `state.State` (`internal/controlplane/state/state.go:19`), `Broker[*pb.Route]` in `watch.Registry` (`internal/controlplane/watch/registry.go:14`), `repeated Route routes = 6` in the `FSMSnapshot` message (`proto/jaco/v1/fsm.proto:12`) with save/restore in `internal/controlplane/fsm/snapshot.go:22,70`, the `RouteEvent` message in `proto/jaco/v1/events.proto:49` (`{EventKind kind; Route before; Route after; uint64 raft_index}`) wired into `SubscribeEvent` as `route = 5` (`proto/jaco/v1/services.proto:241`) and forwarded by `forwardRoutes` (`internal/controlplane/grpc/watch.go:122`).
- The standalone `RouteUpsert`/`RouteRemove` commands (`proto/jaco/v1/commands.proto:179-183`, FSM `fsm.go:279-294`) have **no issuer** outside the FSM/tests — Routes flow only through `DeploymentApply` + the delete cascade. TCP routes follow the same single path.

## §3 Decisions

1. **Where the TCP route lives in state.** Options: new `TCPRoute` entity; extend `Route` with a protocol discriminator; derive from `ContainerSpec.ports` with no entity. **Chosen:** new `TCPRoute` entity. Rationale: `Route` is `(domain,path)`-keyed and carries TLS/path/domain that are meaningless for L4; a materialized entity gives `jaco get` visibility, a store to run the collision check against, and a dedicated watch broker the datapath subscribes to exactly as the Caddy reloader subscribes to Routes.
2. **Entity key / collision policy.** Options: reject at admission; last-writer-wins; namespace per deployment. **Chosen:** key = published host port (cluster-global); **reject at admission** with a structured `port_conflict` when a different deployment already owns that port. Rationale: a node can bind a port once; this mirrors the existing `(domain,path)` duplicate-route reject and surfaces the conflict instead of leaving it undefined.
3. **Re-apply reconcile semantics.** Options: replace-set per deployment; upsert-only (match HTTP today). **Chosen:** replace-set — `DeploymentApply` removes the deployment's existing TCPRoutes then applies the new desired set, so a dropped `ports:` entry tears its listener down cluster-wide. **The same replace-set is extended to HTTP `Route`** in this slice (per user direction), closing the upsert-only prune gap at `fsm.go:160`. Rationale: a stale north-south listener that keeps forwarding after its port is removed is a correctness bug for both L4 and L7.
4. **jaco.yaml surface.** Options: implicit from compose only; implicit + jaco.yaml opt-out; explicit jaco.yaml block. **Chosen:** implicit from compose only. Rationale: the issue states compose `ports:` drives ingress; adds no manifest field, keeping the spec §Security closed field-set intact.
5. **Which compose `ports:` entries qualify.** Options: explicit published TCP port only; any published port (reject ambiguous). **Chosen:** explicit published TCP port only — numeric `Published`, `Protocol == tcp`, no `HostIP` scoping. Bare/target-only (`5432`), ephemeral, UDP, and `127.0.0.1:`-scoped entries produce no listener, preserving the prior "documentation only" meaning and back-compat with existing compose files.
6. **Derivation point.** Options: at admission from the parsed project; lazily in the FSM/daemon from stored `compose_yaml`. **Chosen:** at admission, in a new `toTCPRoutes(deployment, project)` mirroring `toRoutes`, fed by the `compose.LoadBytes` project (ranges already expanded). Rationale: same layer as `toRoutes`, gives the collision check the parsed project, and keeps the FSM a pure state-writer.
7. **`reserved_port` validation home.** Options: `compose.Validate` (raw bytes); post-load on the typed project. **Chosen:** `compose.Validate`, matching the issue's named home and `validate_test.go` coverage; it hand-parses the published side of each `ports:` entry (incl. `H1-H2` ranges) just as it already hand-parses `networks:`. Rationale: keeps the typed `reserved_port` error on the existing raw-YAML validator that runs before `LoadBytes`.

## §4 Contracts & shapes

**Proto (`proto/jaco/v1/`)**

- New `entities.proto` message `TCPRoute`: `{ int32 published_port = 1; string deployment = 2; string service = 3; int32 container_port = 4; }`. TCP-only by construction — no `protocol` field (no UDP caller; see §6). Key is `published_port` alone.
- `commands.proto` `DeploymentApply`: add `repeated TCPRoute tcp_routes = 7;` (alongside `repeated Route routes = 6`).
- `fsm.proto` `FSMSnapshot` message (`fsm.proto:12`): add `repeated TCPRoute tcp_routes = 16;` after `audit_events = 15`.
- `SubscribeEvent` (`services.proto:236`): add `TCPRouteEvent tcp_route = 15;`. Define the `TCPRouteEvent` message in `events.proto` (where `RouteEvent` actually lives, `events.proto:49`) mirroring `RouteEvent`'s real shape: `{ EventKind kind = 1; TCPRoute before = 2; TCPRoute after = 3; uint64 raft_index = 4; }`.

**State (`internal/controlplane/state/`)**

- New `TCPRoutes *Store[*pb.TCPRoute]` in `state.State`, constructed in `New(...)`.
- New `tcproutes.go` with `TCPRouteKey(publishedPort int32) string` (decimal string) and the store constructor keyed on `r.GetPublishedPort()`.

**Watch (`internal/controlplane/watch/`)**

- New `TCPRoutes *Broker[*pb.TCPRoute]` in `Registry`, sized to `DefaultBuffer` in `NewRegistry`.
- New `forwardTCPRoutes` in `internal/controlplane/grpc/watch.go` mirroring `forwardRoutes`, with the same `depFilter` behavior, emitting `SubscribeEvent_TcpRoute`.

**Derivation (`internal/controlplane/grpc/`)**

- New `toTCPRoutes(deployment string, project *composetypes.Project) []*pb.TCPRoute` in `jaco_spec.go`: for each service, for each `ServicePortConfig` where `Published` parses to a positive int, `Protocol == "tcp"` (or empty → tcp), and `HostIP` is empty/`0.0.0.0`, emit `TCPRoute{published_port: Published, deployment, service: svc.Name, container_port: Target}`. Skip everything else.
- Collision check in `deploy.Apply` after `parseAndValidate`, before building the command:
  - intra-apply: two qualifying entries (across services in this compose) with the same `published_port` → `InvalidArgument` `port_conflict`, message names both services + the port.
  - cross-deployment: any `published_port` already present in `state.TCPRoutes` whose owning `deployment` differs from the one being applied → `InvalidArgument` `port_conflict`, message names the conflicting deployment + the port. A re-apply of the *same* deployment re-claiming its own ports is not a conflict.
- `DeploymentApply` command packs `TcpRoutes: toTCPRoutes(jacoSpec.Deployment, composeProject)`.

**FSM (`internal/controlplane/fsm/fsm.go`)**

- `DeploymentApply` becomes replace-set for both route kinds:
  - HTTP: remove every `Route` where `deployment == da.Deployment`, then `Routes.Apply` each `da.GetRoutes()`.
  - TCP: remove every `TCPRoute` where `deployment == da.Deployment`, then `TCPRoutes.Apply` each `da.GetTcpRoutes()`.
- `DeploymentDelete`: add a cascade loop removing `TCPRoute`s by deployment, beside the existing Routes cascade (`fsm.go:189-193`).
- `snapshot.go`: include `TCPRoutes.List()` in save and re-`Apply` on restore, beside Routes (`:22,70`).

**Compose validation (`internal/runtime/compose/validate.go`)**

- New per-service `ports:` inspection in `Validate`: for each entry, parse the **published host side** (short `"H:C"`, `"H1-H2:C1-C2"`, `"IP:H:C"`; long-form map `{published, target}`). If the published value equals `80` or `443`, or a published range includes either, return `&ValidationError{Code: "reserved_port", Message: fmt.Sprintf("service %q publishes reserved host port %s (entry %q); 80 and 443 belong to JACO's HTTP/S ingress", svc, port, raw), Details: {"service", "port", "entry"}}`.
- Container/target side `80`/`443` is **not** a violation. Bare/target-only entries (no published host side) are **not** a violation (decision 5 — documentation only). First violation wins, deterministic by sorted service name (matches existing ordering at `validate.go:84-89`).
- `port_conflict` surfaces through the existing typed path: `parseAndValidate` already maps `compose.ValidationError` → `pb.Error` (`deploy.go:212-216`); `port_conflict` is returned directly from `deploy.Apply` as a `codes.InvalidArgument` status with that code.

## §5 Sequence

Apply (happy path):
1. `deploy.Apply` runs `parseAndValidate`: `compose.Validate` rejects any `80`/`443` published-port entry with `reserved_port`; on success `LoadBytes` yields the range-expanded project.
2. `toTCPRoutes` projects qualifying published ports into `[]*pb.TCPRoute`.
3. Collision check scans the new set (intra-apply) and `state.TCPRoutes` (cross-deployment); any clash → `port_conflict`, no state change.
4. `DeploymentApply` command carries `Services`, `Routes`, `TcpRoutes`; raft-applied.
5. FSM replace-set: prunes the deployment's prior Routes + TCPRoutes, applies the new sets; brokers publish add/remove events.
6. Datapath slice (separate) observes `TCPRoute` events and opens/closes listeners; HTTP reloader observes `Route` events as today.

Re-apply dropping a port:
1. New compose omits a previously-published port → `toTCPRoutes` returns the smaller set.
2. FSM removes all the deployment's TCPRoutes then applies the new set → the dropped port's `TCPRoute` is gone → broker emits Removed → datapath closes that listener cluster-wide.

Delete:
1. `DeploymentDelete` FSM cascade removes the deployment's Deployment, Routes, **TCPRoutes**, ReplicaDesired, Subnets.
2. Broker emits Removed for each TCPRoute → datapath closes listeners.

Collision reject:
1. Deployment B applies a compose publishing `5432`, already owned by deployment A in `state.TCPRoutes`.
2. `deploy.Apply` returns `InvalidArgument` `port_conflict` naming A + `5432`; no command is applied; A keeps its listener.

## §6 Out of scope

- The per-node TCP listener, packet forwarding, upstream load-balancing, TCP health probing, and graceful drain → **datapath** slice.
- UDP. The entity is TCP-only and named `TCPRoute`; a future UDP path is a sibling entity (or a protocol dimension added to the key), not a field retrofitted now — no UDP caller exists.
- Operator override/opt-out of a derived port (decision 4: implicit-from-compose only).
- Listener implementation choice (user-space proxy vs. nftables DNAT) → datapath slice.
- Revision-history-based rollback restoring prior routes — rollback still only flips revision markers (`fsm.go:168-185`); replace-set operates on the current applied revision.

## §7 Open questions

Both prior open items are now resolved (kept here as a decision trail):

- **Bare `"80"`/`"443"` validation — RESOLVED.** A bare port (no colon, e.g. `ports: ["80"]`) is a container/documentation declaration with no host publish, so it is **not** a `reserved_port` violation and creates no listener (confirmed against the issue's "container-side targets are fine" rule and decision 5). Only an explicit published side (`80:...`, `{published:80}`, or a published range covering 80/443) is rejected. Encoded in §4.
- **spec.md §3 reconciliation — RESOLVED.** `spec.md` has been amended: the §3 In `ports` clause now states published host ports drive cluster-wide TCP ingress (80/443 reserved), the "Ingress on every node" bullet covers raw TCP, and a top-level TCP-ingress promise was added mirroring the HTTP one.

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
