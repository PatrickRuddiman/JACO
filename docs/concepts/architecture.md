# Architecture

JACO is a multi-node container orchestrator built on hashicorp/raft,
embedded Caddy, WireGuard, and per-(deployment, network) docker bridges
with nftables-enforced isolation. It ships as **two binaries**:

- `jacod` — the long-running daemon, managed by systemd. Listens on a
  unix socket for local CLI control and on TCP for peer + remote
  control.
- `jaco` — the operator and developer CLI. Talks to a local `jacod`
  over the unix socket and to peer `jacod`s over TCP for cross-host
  control.

This page is the architectural overview. For the source-of-truth design
material aimed at the implementation, see
[`docs/planning/design.md`](../planning/design.md).

## Verticals

Every node runs the same set of verticals inside one `jacod` process.
Each has its own page under [`internal/.../doc.go`](../../internal) and
a more detailed design slice under
[`docs/planning/slices/`](../planning/slices).

| vertical          | responsibility                                                                                       | slice |
|-------------------|------------------------------------------------------------------------------------------------------|-------|
| **control-plane** | raft group, replicated state machine, command admission, watch fan-out, audit log, cluster CA       | [slice](../planning/slices/control-plane.md) |
| **scheduler**     | leader-only desired-state reconciler; placement, rolling updates, drain, restart-after-3            | [slice](../planning/slices/scheduler.md) |
| **runtime**       | per-node docker engine driver; image pull, container lifecycle, healthcheck observation, log tail   | [slice](../planning/slices/runtime.md) |
| **discovery**     | per-node bridges, /24 IPAM, WireGuard mesh, nftables isolation, per-bridge DNS                      | [slice](../planning/slices/discovery.md) |
| **ingress**       | embedded Caddy on `:80, :443`; ACME issuance + renewal via raft-backed CertMagic storage            | [slice](../planning/slices/ingress.md) |
| **daemon**        | `jacod` itself: config loading, lifecycle, goroutine orchestration, admission gating                | [slice](../planning/slices/daemon.md) |
| **cli**           | operator + developer subcommands                                                                    | [slice](../planning/slices/cli.md) |
| **packaging**     | release pipeline, install, `jaco self-upgrade`                                                      | [slice](../planning/slices/packaging.md) |

## Data flow at a glance

1. The CLI submits a write (e.g. `Deploy.Apply`) to any node.
2. The admission interceptor resolves the bearer token to an identity
   (or trusts the unix-socket peer); attaches it to the request context.
3. The handler validates the payload, builds a `Command{}` proto, and
   submits it to raft. Non-leaders forward to the leader via
   `Internal.Submit`.
4. Raft replicates the command to a majority and applies it on every
   node's FSM. The FSM mutates the typed entity store and writes an
   `AuditEvent`.
5. The watch broker publishes a typed `Event<T>` to every subscriber
   (scheduler, runtime, ingress, discovery, `jaco status -w`).
6. Subscribers react: the scheduler diffs `ReplicaDesired`, the
   runtime starts/stops containers, ingress rebuilds Caddy config,
   discovery materializes bridges and DNS responders.

See [Cluster lifecycle](cluster-lifecycle.md),
[Scheduling](scheduling.md), and [Status and errors](status-and-errors.md)
for the moving parts of that flow.

## Replicated state

Canonical entities held in the raft FSM (see
[`proto/jaco/v1/entities.proto`](../../proto/jaco/v1/entities.proto)):

- `ClusterMeta` — cluster id, CA cert, CA key (singleton).
- `Node` — one per cluster member; hostname, addresses, WG pubkey, status.
- `Deployment` — one per `jaco apply`; carries the literal jaco.yaml
  + compose bytes plus the parsed `ServiceSpec` list.
- `ReplicaDesired` — one per `<deployment, service, index>`; the
  scheduler's writable view.
- `ReplicaObserved` — one per replica; the runtime writes state
  transitions (`pending`, `pulling`, `running`, …) back through here.
- `Route` — HTTP(S) ingress entries.
- `TCPRoute` — raw-TCP listeners derived from compose `ports:`.
- `Cert` + `CertBlob` + `ChallengeToken` — TLS material for managed
  domains.
- `Token` — operator-token records (identity + hashed secret).
- `JoinToken` — single-use cluster-membership tokens.
- `Subnet` — per-(deployment, network, host) `/24` allocation.
- `RolloutPlan`, `ReplicaCounter`, `RestartCounter` — scheduler bookkeeping.
- `AuditEvent` — typed audit log.

The set is **closed**: there is no plugin mechanism for new entities in
v1.

## Project status

Pre-release. Functional for single-host and multi-host clusters via the
two-binary path described above. Known open gaps (this is the canonical
list; other pages link here instead of repeating it):

- TLS on the cross-host gRPC listener — currently plaintext, with the
  expectation that operators wrap the wire in an overlay (Tailscale,
  WireGuard, VPC) and authenticate via bearer tokens. Cluster-CA TLS
  with cert pinning lands in a follow-up.
- Follower → leader forwarding of `ReplicaObserved` updates.
- The Caddy v2 ingress reload loop fully integrated with the rebuild
  debounce window.
- Rollout state-machine integration with the scheduler's reconcile.
- The drain step machine for `jaco node remove`.

A handful of CLI subcommands (`rollback`, `delete`, `token *`,
`node list`) currently require `--server`; the unix-socket path for
those RPCs is planned. See the CLI pages for the exact contract today.

## See also

- [Getting started](../getting-started.md)
- [Cluster lifecycle](cluster-lifecycle.md)
- [Networking](networking.md)
- [Repository layout](../contributing/repo-layout.md)
- [`docs/planning/design.md`](../planning/design.md) — full design
- [`docs/planning/spec.md`](../planning/spec.md) — full spec
