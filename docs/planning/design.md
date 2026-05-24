Parent spec: [spec.md](spec.md)

# JACO — design

## §1 Summary

JACO is a single static Go binary running on every cluster node as the `jaco` daemon, with the same binary serving as the CLI. Cluster nodes form a raft consensus group whose replicated state machine holds deployments, services, replicas, routes, certs, tokens, and the audit log. Every node hosts the same set of components in one process: a gRPC API surface (CLI and node-to-node), an embedded Caddy proxy on 80/443, a docker engine driver, a service-DNS responder, and a WireGuard interface for inter-node container traffic. Other verticals consume state through gRPC watch streams sourced from raft commits. The design commits to ⌊(N−1)/2⌋ raft fault tolerance, p95 ingress < 5 ms LAN, ≤10 s leader election, ≤15 s apply-to-steady-state, 5 s revocation propagation, and per-node ingress acceptance for any declared domain.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Daemon and CLI language.** Options: Go (single static binary, subcommands), Rust, Go with two binaries. **Chosen:** Go, single static binary with subcommands. Rationale: serves the §4 Compatibility bar (Linux + macOS, x86_64 + arm64 cross-compiled binaries) and the §4 Performance bars (low-latency hot paths in ingress and watch streams) with first-class raft and docker-SDK ecosystems. Affects: every vertical.

2. **Reverse proxy for ingress.** Options: Caddy embedded as a Go library, Caddy as a sidecar process, Traefik file provider, custom net/http + lego. **Chosen:** Caddy embedded as a Go library (`caddyserver/caddy/v2`). Rationale: serves the §1 promise "TLS certificates obtained, installed, and renewed without operator action" and "any node accepts ingress traffic for any declared domain" with the fewest moving parts per node — one process, no IPC, ACME built in. Affects: ingress, control-plane, cli.

3. **East-west networking for service discovery and isolation.** Options: node-local TCP proxy + DNS, WireGuard mesh + flat CIDR + DNS, docker overlay driver, WireGuard mesh + per-(deployment, network) bridges + DNS. **Chosen:** WireGuard mesh + one docker bridge per (deployment, network) per node, each with its own /24 from the cluster CIDR + per-node DNS responder + JACO-managed nftables rules enforcing the isolation boundary. Rationale: serves the spec's cross-deployment isolation and per-deployment compose-network semantics; encrypted inter-node container traffic; each isolation boundary is enforced by both bridge separation (L2) and nftables (L3) — both mechanisms required to be operational before a node is considered ready (spec §2 failure mode). This is the largest single engineering surface in v1; the slice spells out atomic ruleset reload, docker-iptables coexistence, and the CI test rig required to verify the boundary holds. Affects: discovery, runtime, control-plane.

4. **CLI ↔ cluster transport and identity carrier.** Options: gRPC + server-auth TLS + opaque bearer token, HTTP/JSON + bearer token, gRPC + mTLS client cert. **Chosen:** gRPC over server-auth TLS with opaque bearer tokens looked up in replicated state. Rationale: directly serves the §4 Security bar "token revocation effective cluster-wide within 5 s" — revocation is a raft write applied on every node, well under 1 s. gRPC streaming supports watch, log fanout, and apply progress on the same transport. Affects: cli, control-plane, security contract.

5. **State change propagation to scheduler/runtime/ingress.** Options: gRPC watch streams, revision-aware long-poll, periodic full-state pull. **Chosen:** gRPC watch streams subscribed to slices of replicated state. Rationale: serves the §4 Reliability and Performance bars that depend on sub-second cluster reaction — "removes failing replica from routing within 5 s", "new leader elected within 10 s", "apply to steady state within 15 s". Affects: scheduler, runtime, ingress, cli, control-plane.

6. **Status and error envelope shape.** Options: typed code + message + details bag, free-form string, typed code only. **Chosen:** `{code: enum, message: string, details: map<string,string>}` for both status and error. Rationale: spec §2 and §3 enumerate closed sets for replica statuses (`pending, running, degraded, updating, failed, stopped`) and rejection reasons (`unknown service`, `unknown host`, `cannot place N replicas on M pinned hosts`, etc.); a typed envelope makes those sets enforceable cross-vertical and filterable in `jaco status`. Affects: cli, control-plane, scheduler, runtime, ingress.

7. **Log streaming fanout.** Options: server-side fanout over node-to-node gRPC, client-side fanout from CLI to every node, central log aggregator. **Chosen:** server-side fanout. The entry node opens peer streams to every node hosting a target replica and merges into the CLI stream in arrival order. Rationale: serves the spec's "any node accepts the request" promise for `jaco logs` and the §2 acceptance line on per-replica order with arrival-time interleave. Affects: cli, runtime, control-plane.

8. **Packaging and self-upgrade.** Options: single static Go binary + curl install + systemd unit, two binaries, container image, distro packages. **Chosen:** single static binary + release tarball + `install.sh` + systemd unit, with a `jaco self-upgrade <url>` subcommand. Rationale: serves the §2 operator story "upgrade JACO on a single node without bringing down the cluster" — swap binary, restart systemd unit, wait for raft rejoin, repeat. CLI and daemon ship as the same artifact so version skew is impossible. Affects: packaging, control-plane.

9. **Observability for the daemon itself.** Options: Prometheus `/metrics` + JSON logs, OpenTelemetry (OTLP), logs-only. **Chosen:** OpenTelemetry via OTLP, covering metrics, traces, and logs. Rationale: end-to-end traces (`cli.apply → raft.commit → scheduler.reconcile → runtime.start_container`) attribute slow applies to a specific stage, which is essential for diagnosing the §4 Performance bars from production. OTLP endpoint is operator-configurable; SDK is disabled if endpoint is unset. Affects: every vertical.

## §4 Verticals

- **cli** — single-binary subcommands for operator and developer: `serve`, `bootstrap`, `node {join,remove,list}`, `apply [--dry-run]`, `rollback`, `delete`, `status`, `logs`, `audit [--since,--type]`, `token {issue,revoke,list}`, `backup`, `restore`, `self-upgrade`.
- **control-plane** — raft group, replicated state machine, leader election, command admission and identity injection, audit log writer, watch-stream multiplexer, cluster CA, snapshot + backup/restore.
- **scheduler** — desired-state reconciler driven by control-plane watches: placement (`hosts`, `placement: spread|pack`), one-at-a-time rolling updates that never drop below `replicas − 1`, drain-and-replace on graceful node remove, healthcheck-driven reroute + restart-with-fail-after-3, rollback to previous applied jaco.yaml.
- **runtime** — per-node docker engine driver: image pull with backoff, container lifecycle, healthcheck probe execution (compose `healthcheck` honored; compose `restart` ignored), local-replica log tail, named-volume and bind-mount handling.
- **ingress** — embedded Caddy on :80/:443 on every node: route config and TLS material propagated from control-plane via watch; HTTP-01 ACME challenges; healthy-upstream selection; HTTP 502 on no healthy upstream.
- **discovery** — per-node DNS responder + WireGuard mesh: cluster CIDR with per-node subnet, container IPs assigned within that subnet, WireGuard peers maintained from control-plane state, DNS answers `<service>` from a local cache fed by replica watches.
- **packaging** — single static `jaco` binary, release tarball with `install.sh` (places binary in `/usr/local/bin`, installs systemd unit), uninstall script, `jaco self-upgrade <url>` for in-place daemon swap on one node.

## §5 Cross-cutting contracts

Replicated state shape (canonical entities in the raft state machine):

- `Node {hostname, address, status ∈ {leader,follower,candidate,joining,leaving,offline}, wireguard_pubkey, joined_at}`
- `Deployment {name, compose_ref, services: map<string, ServiceSpec>, routes: list<Route>, applied_revision, previous_revision}`
- `ServiceSpec {replicas: int≥0, hosts: list<hostname>?, placement ∈ {spread,pack}}` — `hosts` and `placement` are mutually exclusive; absent → `spread`.
- `ReplicaDesired {id, deployment, service, host, image, env, healthcheck, volumes}`
- `ReplicaObserved {id, state: enum, container_id, started_at, last_health_at, message, details}`
- `Route {domain, service, port, tls ∈ {auto,off}, cert_state ∈ {pending,issued,renewing,failed}}`
- `Cert {domain, private_key, cert_chain, issued_at, expires_at, last_error}` — replicated to every node; never leaves the cluster boundary.
- `Token {identity, hashed_secret, issued_at, revoked_at?}`
- `AuditEvent {ts, type, identity, payload}` — type is from the closed set in spec §4 Security.
- `Subnet {deployment, network, cidr}` — one entry per declared (deployment, network) pair; `cidr` is a /24 allocated from the cluster pool.

gRPC service surface (single proto package, used by CLI and node-to-node):

- `Cluster.{Bootstrap, NodeJoin, NodeRemove, Status, Backup, Restore}`
- `Deploy.{Apply, Rollback, Delete, Status, Logs(server-stream)}`
- `Audit.{Query, Tail(server-stream)}`
- `Token.{Issue, Revoke, List}`
- `Watch.{Subscribe(server-stream)}` — internal, consumed by scheduler/runtime/ingress; not exposed to CLI as a primary surface.
- All write RPCs go through the current raft leader; non-leader nodes proxy transparently.

Identity propagation:

- CLI presents an opaque token in gRPC metadata `authorization: Bearer <token>`.
- Entry node validates by looking up the token's hash in replicated state; resolves to an identity name or rejects with `token_invalid` / `token_revoked`.
- Identity name rides on the request context and is recorded in every `AuditEvent` the command produces.
- Revocation is a state machine write; the lookup is consistent across nodes within one raft commit-and-apply (well under 5 s, satisfying spec §4).

Status / error envelope:

- `Status {code: enum, message: string, details: map<string,string>}`
- `Error {code: enum, message: string, details: map<string,string>}`
- Closed enums:
  - replica state ∈ {pending, pulling, running, degraded, updating, failed, stopped}
  - error code ∈ {unknown_service, unknown_host, replicas_exceed_pinned_hosts, image_pull_failed, quorum_lost, no_leader, token_revoked, token_invalid, cert_failed, docker_error, node_already_member, validation_failed, internal}

Cluster CA and node certs:

- `jaco bootstrap` on the first node generates a self-signed cluster CA. The CA private key lives in raft state (replicated to every node); it never leaves the cluster.
- Each node generates its own server cert signed by the cluster CA on first boot; the node-local key never leaves the node.
- `jaco node join` on a new node carries the cluster CA cert (public material) plus a single-use join token; the joining node then mints its own server cert.

WireGuard mesh + bridge isolation contract:

- Cluster CIDR defaults to `10.42.0.0/16` (operator-configurable at bootstrap); leader carves it into /24s on demand, one /24 per (deployment, network) entry, stored as `Subnet{deployment, network, cidr}` in raft state.
- Every (deployment, network) compose pair is materialized as a docker bridge on every node that runs a replica of any service attached to that network. Bridge name: `jaco-<deployment>-<network>` (truncated to 15 chars per kernel limit; suffixed with a 4-char hash if collision). All bridges hosting the same (deployment, network) /24 share that subnet.
- Each node publishes its WireGuard public key on join; peers update mesh config from a watch on `Node`. WG `AllowedIPs` per peer is the union of `Subnet.cidr` for every (deployment, network) that peer hosts a replica for.
- nftables rules (managed by JACO) enforce: traffic between bridges with matching `(deployment, network)` labels is allowed; cross-deployment or cross-network traffic is dropped at FORWARD; node-host services not reachable from JACO bridges except the DNS responder.
- nftables ruleset is keyed by named sets `dep_net_<deployment>_<network>` containing the (deployment, network)'s subnet CIDR. The FORWARD chain matches packets where `ip saddr ∈ set X AND ip daddr ∈ set X` (any matching set → ACCEPT). Final rule: DROP. Same set construction works for same-node bridge-to-bridge and for cross-node traffic decrypted from `wg-jaco` (which arrives without a JACO bridge as iifname).
- Ruleset reload is atomic via `nft -f`: JACO renders the full expected ruleset for the node on every Subnets/ReplicaObserved/Nodes watch event (debounced 200 ms), submits as one transaction. Partial state is impossible.
- Docker iptables coexistence: JACO runs docker with default iptables management (NAT for outbound, per-bridge isolation). JACO adds its rules in a separate nftables table `inet jaco`; iptables-nft compatibility means both layers run in parallel chains. JACO rules sit in `forward` and `input` hooks of the `inet jaco` table; precedence is fine because JACO chains evaluate in addition to docker's iptables chains, and a DROP from any chain stops the packet.
- A node is "ready" only after the nftables ruleset has loaded successfully at startup and a self-test packet (synthetic same-network ACCEPT + cross-deployment DROP) has been verified via `nft -n list ruleset`. Until then, the node refuses to schedule replicas and `jaco status` reports `isolation_unavailable`.
- DNS responder listens on a fixed link-local address per bridge (`10.<deployment-idx>.<network-idx>.1` derived from the bridge's /24); container resolvers point at it; answers `<service>` and `<service>.jaco.local` from a watch-fed cache of `ReplicaObserved` filtered to services on the same (deployment, network).

OpenTelemetry contract:

- All RPCs (CLI ↔ node and node ↔ node) propagate W3C traceparent.
- Span names: `cli.<command>`, `raft.commit`, `raft.apply`, `scheduler.reconcile.<service>`, `runtime.<docker_op>`, `ingress.request`, `cert.<acme_op>`.
- Required span attributes: `jaco.cluster_id`, `jaco.node`, `jaco.deployment`, `jaco.service`, `jaco.replica_id`, `jaco.identity`.
- OTLP exporter endpoint from env `JACO_OTLP_ENDPOINT`; SDK disabled when unset.

## §6 Failure modes & rollout

Spec failure modes → design-level handling:

- **Leader unreachable** → CLI's gRPC client re-resolves to a peer node from its known address list; the peer node proxies to the current leader via the internal gRPC. If no leader within 10 s, the call returns `Error{code: no_leader}` and exits non-zero. Trace span records the retry chain.
- **Network partition** → minority side rejects writes with `quorum_lost`; reads served from local watch cache. Ingress on the minority side continues for routes whose upstreams have replicas still running locally.
- **Image pull failure** → runtime writes `ReplicaObserved{state: failed, code: image_pull_failed, details: {reason}}`; scheduler does not promote to running; prior rolling-update step's replicas keep serving.
- **Healthcheck failure** → runtime updates `ReplicaObserved` state; ingress watch removes the replica from the upstream pool within 5 s; scheduler issues restart; after 3 consecutive restart failures, state becomes `failed` and the replica is not retried until the next apply.
- **TLS issuance failure** → cert state stays `pending` in `Route`; control-plane retries with exponential backoff capped at 1 h; ingress continues serving plain HTTP for that route.
- **Docker daemon down on a node** → runtime watch loop reports `docker: unreachable` to control-plane; scheduler reschedules replicas to eligible nodes; ingress on that node continues forwarding for routes whose targets resolve to remote replicas.
- **Token revoked mid-call** → entry node's admission check reads current token state at command time; revocation propagated within one raft apply (well under 5 s).
- **Concurrent applies on same deployment** → serialized by the raft log; the second observes the first's post-state.
- **Isolation ruleset fails to load (no nftables, transaction error, kernel mismatch)** → daemon refuses to enter ready state; node does not schedule replicas; `jaco status` reports `isolation_unavailable` with reason; operator must repair before the node can host workloads. Discovery slice runs a 30 s reconcile loop that re-validates the ruleset against the expected one and emits an `isolation_ruleset_reconciled` audit event on any out-of-band drift it corrects.

Observability surface for operators:

- Metrics (OTel): `jaco_raft_commit_latency_seconds`, `jaco_apply_duration_seconds`, `jaco_scheduler_reconcile_lag_seconds`, `jaco_replica_state{service,state}`, `jaco_ingress_requests_total`, `jaco_ingress_duration_seconds`, `jaco_cert_renewals_total`, `jaco_runtime_container_starts_total`, `jaco_token_revocation_propagation_seconds`.
- Traces: end-to-end from `cli.apply` through `raft.commit` → `scheduler.reconcile` → `runtime.start_container`, so a slow apply attributes to a stage. Same chain instruments rollback and delete.
- Logs: structured (JSON when OTel disabled, OTLP logs when enabled). Always written to stdout; systemd captures.

Rollout for v1 (no in-place migration; greenfield):

- Operators install fresh on each node via `install.sh`. First node runs `jaco bootstrap`; subsequent run `jaco node join`.
- Self-upgrade procedure: on one node at a time, `jaco self-upgrade <url>` downloads the new binary, swaps `/usr/local/bin/jaco`, restarts the systemd unit, and waits for raft rejoin (≤10 s) before reporting done. Operator advances to the next node when `jaco status` reports the upgraded node back as a follower or leader.
- The CLI subcommand of an N+1 binary must accept commands against an N daemon within the same major version; gRPC field additions are backward-compatible by design.

## §7 Out of scope (architecture-level)

- A plugin or extension system for verticals — the entity set in §5 is closed.
- Multi-cluster federation — one raft group per cluster.
- A separate state store outside raft — no etcd, no Postgres, no Consul.
- Sidecar processes for ingress, runtime, or discovery — all components live in the single `jaco` process per node.
- Image building (consumes registries only).
- User-defined network policies beyond compose `networks:` semantics (no NetworkPolicy-style intra-network rules in v1).
- A standalone observability daemon — OTel SDK lives in the JACO process.

> If the parent spec is ambiguous on anything this design depends on, stop and update the spec. Do not invent behavior here.
