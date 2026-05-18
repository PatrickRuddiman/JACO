Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — ingress

## §1 Summary

Per-node north-south HTTP(S) router. Embeds Caddy as a Go library inside the JACO daemon; subscribes to Routes, Certs, and ReplicaObserved watches; rebuilds Caddy's running config on every relevant change. Owns ACME issuance/renewal via a custom certmagic Storage backed by raft state. Serves 80 and 443 on every node for every declared domain in the cluster.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Caddy config push.** Options: in-process `caddy.Run/Load` Go API, admin API loopback HTTP, file provider + watch. **Chosen:** in-process `caddy.Load` via the Go API. Rationale: no IPC, no port to harden; lowest-latency config swap; same Go runtime so panics surface in the daemon.
2. **ACME challenge.** Options: HTTP-01 (with cluster-wide token coordination), HTTP-01 + TLS-ALPN-01, DNS-01. **Chosen:** HTTP-01 only with raft-coordinated challenge tokens. Rationale: matches the spec's "DNS resolves to cluster node IPs" model; works on any node that receives the validation request without a DNS provider integration.
3. **Upstream load balancing.** Options: random + passive health, round-robin, least-connections. **Chosen:** random with passive health failover. Rationale: cheapest; the spec promises "reach a healthy replica" not a specific distribution; passive health uses Caddy's existing facility.
4. **Cert storage.** Options: custom `certmagic.Storage` backed by raft, default file storage + sync sidecar, external shared backend. **Chosen:** custom `certmagic.Storage` backed by control-plane's `Cert` entity. Rationale: spec promises "any node accepts ingress for any declared domain" and "TLS private keys never leave the cluster" — both fall out naturally when raft is the storage layer.

## §4 Contracts & shapes

Module layout under `internal/ingress/`:

- `internal/ingress/ingress.go` — boots embedded Caddy at daemon start; opens watches; calls `rebuild()` on relevant events with debounce (200ms).
- `internal/ingress/config.go` — `BuildCaddyConfig(routes, replicas) []byte` — pure function from state to Caddy JSON.
- `internal/ingress/storage.go` — implements `certmagic.Storage`: Store, Load, Delete, Exists, List, Stat, Lock, Unlock. Lock/Unlock writes go through raft so issuance is single-flight cluster-wide per domain.
- `internal/ingress/challenge.go` — HTTP-01 token coordination: when certmagic asks Caddy to serve a challenge, JACO writes the token+keyAuth pair to raft as an ephemeral `ChallengeToken{domain, token, key_auth, expires_at}`. The HTTP-01 handler on every node reads the local watch cache to answer challenge requests received by any node.

Caddy config shape produced per rebuild (JSON, fed to `caddy.Load`):

- Single `apps.http.servers.jaco` server listening on `:80, :443`.
- One route per declared `Route` entity in raft state; route matcher = host header.
- Upstreams = list of `{dial: "<host>:<port>"}` for each healthy `ReplicaObserved` of the target service, where "healthy" means `state ∈ {running}` AND `last_health_at within 10s`.
- TLS automation policy: `on_demand: false`, `issuers: [acme{ca: <Let's Encrypt prod URL>, email: <operator email or empty>}]`, `key_type: p256`, `storage: jaco` (the custom storage).
- Reverse-proxy load-balancing: `{selection_policy: {policy: random}, retries: 2, fail_duration: 10s}`.
- Static fallback route for unknown hosts: returns 404 with `Server: jaco` header.

Custom `certmagic.Storage` semantics:

- `Store(key, value)` → raft Apply `{op: CertStore, key, value, ttl?}`. Persisted under `Cert{domain}` for cert/key blobs; under `ChallengeToken{token}` for HTTP-01 challenges.
- `Load(key)` → read from local in-memory typed store (kept in sync by watch); fall back to leader-read if not present locally and watch is mid-Resync.
- `Lock(name)` → raft Apply `{op: CertLock, name, lessee: <node>, until: now+5min}`; fails if a non-expired lock exists for another lessee. Renews automatically every 2min while held.
- `Unlock(name)` → raft Apply `{op: CertUnlock, name}`.
- `List(prefix, recursive)` → enumerates local store under prefix.
- `Delete`, `Exists`, `Stat` — straightforward local reads/Applies.

Route entity → Caddy mapping (closed):

- `Route{domain, service, port, tls: auto}`:
  - HTTP listener: redirect to HTTPS unless ACME challenge path `/.well-known/acme-challenge/*`.
  - HTTPS listener: TLS with cert from custom storage; reverse proxy to upstreams.
- `Route{domain, service, port, tls: off}`:
  - HTTP listener: reverse proxy to upstreams.
  - No HTTPS listener; no cert.

Replica-observed → upstream eligibility (closed):

- Include `ReplicaObserved` where `state = running` AND `now - last_health_at < 10s`.
- Exclude on any other state (`pending`, `pulling`, `degraded`, `failed`, `updating`, `stopped`).

## §5 Sequence

Daemon startup:

1. `jaco serve` constructs `Ingress`, registers the custom certmagic Storage under name `jaco`.
2. Opens watches on Routes, ReplicaObserved (filtered to services referenced by Routes), Certs, ChallengeTokens.
3. After watch catch-up, computes initial Caddy config and calls `caddy.Load(config)`.
4. Caddy starts listeners on `:80, :443`; ACME issuance is automatically queued for any `tls: auto` route without an issued cert.

Rebuild loop:

1. Watch event arrives (route added, replica state changed, cert state changed).
2. Debounce 200ms (coalesce bursts during scheduler rolling updates).
3. Recompute config; if structurally identical to current (deep compare), skip.
4. Else call `caddy.Load(new_config)`; Caddy applies the diff (graceful listener swap if needed).

ACME issuance (HTTP-01, single domain, cluster-wide coordination):

1. Caddy on node A starts ACME flow for `example.com`.
2. certmagic.Storage.Lock(`issue_lock_example.com`) → JACO writes raft lock; granted to node A.
3. certmagic asks Caddy to serve a token at `http://example.com/.well-known/acme-challenge/<token>`.
4. JACO `challenge.go` writes `ChallengeToken{domain: example.com, token, key_auth, expires_at: now+10min}` to raft.
5. Public CA hits one of the cluster nodes (DNS resolved to any node IP). HTTP-01 handler on whichever node received the request reads its local `ChallengeToken` cache (kept warm by the watch); serves the key_auth.
6. CA validates, returns cert chain to certmagic on node A.
7. certmagic calls Storage.Store for cert+key under `Cert{example.com}`; JACO writes to raft.
8. Watch fires on every node; ingress rebuilds with the new cert in place.
9. Storage.Unlock releases the issue lock.

ACME renewal:

- certmagic's renewal scheduler runs on every node. Storage.Lock prevents thundering herd: only one node performs the renewal, others observe the new cert via watch.
- Renewal threshold: certmagic default (1/3 of remaining validity).
- Failure: cert state in raft moves to `renewing → failed` after backoff cap (1h); existing cert continues to serve until expiry; ingress emits log + audit event `certificate failed`.

Health-driven upstream removal:

1. Scheduler writes `ReplicaObserved{state: degraded}` (or `failed`) for a replica.
2. Ingress watch fires; debounce 200ms.
3. Rebuild excludes that replica from upstream list.
4. Caddy gracefully drains in-flight connections to the removed upstream; new requests skip it.
5. Spec promise of "within 5s" met by debounce window + Caddy graceful swap.

Route delete (deployment delete):

1. Watch: `Event<Route>{Removed}` for every route in the deleted deployment.
2. Rebuild produces a config without those routes.
3. certmagic Storage.Delete called for the cert keys; raft state removes Cert entities; ACME renewal stops.

## §6 Out of scope

- East-west (service-to-service) traffic (lives in discovery slice).
- WebSocket / HTTP/2 / HTTP/3 specifics beyond what Caddy enables by default.
- Custom middleware (auth, rate limiting, header rewriting) — spec §3 Out: "Authentication of end-user HTTP traffic" and the closed routes schema forbid these.
- Wildcard or multi-domain SAN certs (spec routes are one-domain-per-entry; multi-domain comes via multiple Route entries).
- Operator-supplied (non-ACME) certs (spec: only public-CA via ACME).
- Cert revocation propagation back to CRL/OCSP for end clients (Caddy/certmagic handle the standard cert lifecycle).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
