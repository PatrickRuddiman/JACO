---
sources:
  - internal/ingress/
  - internal/daemon/grpc/ingress.go
  - internal/controlplane/grpc/jaco_spec.go
  - proto/jaco/v1/entities.proto
---

# Ingress

JACO's north-south plane: embedded Caddy on `:80` and `:443` on every
node, ACME via a raft-backed CertMagic storage, route definitions
sourced from `Route` entities. Plus a raw-TCP L4 router on every node
for compose-declared `ports:` entries.

Code: [`internal/ingress/`](../../internal/ingress).

## What every node listens on

- `:80` and `:443` — Caddy reverse-proxy for declared HTTP(S) routes.
- Any compose-published host port (e.g. `6379`) — JACO's L4 router
  forwards to a healthy replica of the target service, wherever it
  runs. Reserved ports `80` and `443` belong to Caddy and are
  rejected at apply.

## Route → Caddy mapping

For each `Route` entity in raft state:

- `Route{domain, service, port, tls: auto}`:
  - HTTP listener: redirects to HTTPS, except `/.well-known/acme-challenge/*`.
  - HTTPS listener: TLS with cert from the custom storage; reverse
    proxy to the upstreams.
- `Route{domain, service, port, tls: off}`:
  - HTTP listener: reverse proxy to the upstreams.
  - No HTTPS listener; no cert.

Multiple routes for the same domain with different `path` prefixes
co-exist; Caddy is fed routes longest-prefix-first so the more
specific path wins.

### Path stripping

When a route sets `strip_path: true` and `path` is non-empty, JACO
renders a Caddy `rewrite` handler ahead of the reverse proxy that
strips the matched prefix from the request URI before it reaches the
upstream. With `path: /api` and `strip_path: true`, an inbound `GET
/api/foo?x=1` arrives at the container as `GET /foo?x=1`. The query
string is preserved; only the path prefix is removed.

`strip_path` has no effect when `path` is empty (a catch-all route has
nothing to strip) and defaults to `false`, which forwards the original
URI byte-for-byte — the historical behavior. Declare it in
`jaco.yaml`'s `routes` block; see
[`jaco.yaml` schema](../manifests/jaco-yaml.md).

A static fallback route for unknown hosts returns HTTP 404 with a
`Server: jaco` header.

## Upstream eligibility

An upstream `{dial: "<host>:<port>"}` is included only when its
`ReplicaObserved.state = running` **and** `now - last_health_at < 10s`.
Replicas in `pending | pulling | degraded | failed | updating |
stopped` are excluded. The watch debounce window (200 ms) means a
failing replica is dropped from the upstream pool within roughly
5 seconds end-to-end, satisfying the spec's bar.

Load-balancing across upstreams is **random** with 2 retries and a
10 s failover window. Random matches the spec promise ("reach a
healthy replica") without committing to a specific distribution
policy.

## ACME issuance

JACO uses **HTTP-01 only**, with raft-coordinated challenge tokens so
the public CA can hit any node and the challenge resolves correctly.

Per-domain flow (single-flight cluster-wide):

1. Caddy on some node starts the ACME flow for `example.com`.
2. The custom CertMagic Storage calls `Lock(issue_lock_example.com)` —
   a raft write claims the lock for that node for 5 minutes; renews
   every 2 minutes while held. Other nodes see the lock and stand down.
3. CertMagic asks Caddy to serve a token at
   `http://example.com/.well-known/acme-challenge/<token>`. JACO
   writes the `{token, key_auth, expires_at}` triple to raft as a
   `ChallengeToken` entity.
4. The public CA hits *any* node (DNS resolves to any cluster IP).
   The HTTP-01 handler on that node reads its local `ChallengeToken`
   cache (kept warm by the watch) and serves `key_auth`.
5. The CA validates, returns the cert chain. CertMagic writes
   cert+key through the custom storage to raft (`Cert{domain}`).
6. Watch fires on every node; ingress rebuilds with the new cert.
7. `Unlock` releases the issue lock.

### Stage-first dry run

By default, new domains issue against Let's Encrypt staging first,
then flip to the production directory on success. Staging has much
looser rate limits, so a DNS or firewall misconfiguration burns a
cheap staging failure instead of a prod rate-limit hit. Disable with
[`acme_skip_staging: true`](../configuration.md) in `jacod.yaml`.

### Renewal

CertMagic's renewal scheduler runs on every node. The lock prevents a
thundering herd: only one node performs the renewal; others observe
the new cert via watch.

### Per-stack ACME contact email

A stack's `jaco.yaml` may set a top-level `acme_email:` (issue #102).
When set, that stack's `tls: auto` domains register and renew under
that contact instead of the cluster-wide `acme_email` from
`jacod.yaml`. The rendered Caddy config groups domains by
`(staging, effective-email)` so each unique non-empty email gets its
own automation policy and its own ACME account; stacks that omit the
field fall into the cluster-default policy.

- Two stacks that share an email collapse into one policy (one ACME
  account).
- Changing a stack's `acme_email` triggers a new ACME account
  registration on the next issuance / renewal; the existing valid
  cert keeps serving until renewal.
- The cluster-wide opt-out (`acme_enabled: false`) still wins — no
  automation block at all is emitted, regardless of per-stack emails.

Renewal threshold: CertMagic default (~1/3 of remaining validity).
On failure, cert state in raft transitions `renewing → failed` with
exponential backoff capped at 1 hour; existing cert continues to
serve until expiry. An `AuditEvent{type: certificate_failed}` is
recorded.

## Custom CertMagic storage

The CertMagic `Storage` interface is implemented against raft, with an
optional on-disk fallback cache rooted at `$dataDir/ingress/cache`
(raft stays authoritative):

- `Store(key, value)` — raft Apply (persisted under `CertBlob{}`), then
  a best-effort write-through to the disk cache.
- `Load(key)` — read the in-memory typed store (kept in sync by watch);
  if raft has no copy, fall back to the disk cache.
- `Lock(name) / Unlock(name)` — raft Apply with lessee + expiry.
- `Delete, Exists` — raft Apply / local read, with the disk cache
  consulted (Exists) or cleared (Delete) to match.

### Read-repair from the disk cache (issue #65)

When `Load` finds a blob in the disk cache that raft does not have —
for example raft state was wiped or the node reinstalled while the cert
cache on disk survived — it **re-seeds raft** with that blob before
returning it. This matters because a follower can only serve the
replicated `CertBlob` (it cannot write raft, and it never reads another
node's local disk cache): without the re-seed the leader would serve
TLS from its disk cache while every follower failed. The Apply is a
no-op on a follower (not leader); the leader's `Load` performs the
repair, and once raft holds the blob the fallback branch is no longer
taken.

This is what makes the spec promise hold — "any node accepts ingress
for any declared domain" and "TLS private keys never leave the
cluster" fall out naturally when raft is the storage layer.

## Rebuild loop

A 200 ms debounced rebuild watches `Routes`, `ReplicaObserved` (for
target services), `Certs`, and `ChallengeTokens`. On any change:
recompute the Caddy config; if structurally identical to the running
config, skip; else `caddy.Load(new_config)` — Caddy applies the diff
and gracefully swaps listeners as needed.

## Failure modes

- **No healthy upstream** — Caddy returns HTTP 502 with the
  `Server: jaco` header. `jaco status <dep>/<svc>` reports the
  unreachable target.
- **TLS issuance failure** — `cert_state = pending`; plaintext HTTP
  for the domain continues to serve; backoff capped at 1 h.
- **Cluster-wide ACME disabled** — set `acme_enabled: false` in
  `jacod.yaml`; the rendered Caddy config carries no `tls.automation`
  block. Useful when you front the cluster with your own cert
  pipeline.

## What's out of scope (and where to look instead)

- Custom middleware (auth, rate limiting, header rewriting) — not in
  the closed routes schema. End-user auth is up to the service.
- Wildcard / SAN certs — one domain per route entry; multi-domain via
  multiple entries.
- Operator-supplied (non-ACME) certs — disable ACME and front with
  your own terminator instead.
- WebSocket / HTTP/2 / HTTP/3 specifics beyond what Caddy enables by
  default.

## See also

- [`jaco.yaml` schema](../manifests/jaco-yaml.md) — the `routes`
  block
- [Networking](networking.md), [Isolation](isolation.md)
- [Configuration](../configuration.md) — `acme_*` keys
