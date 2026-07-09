---
sources:
  - internal/ingress/
  - internal/daemon/grpc/ingress.go
  - internal/daemon/grpc/server.go
  - internal/daemon/grpc/apply_or_forward.go
  - internal/controlplane/grpc/jaco_spec.go
  - internal/controlplane/grpc/status.go
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

New domains issue against Let's Encrypt **staging** first; the daemon
runs a cheap self-check on the issued chain (parse + SAN match,
`internal/ingress/stagefirst/stagefirst.go:SelfCheck`); on success it
flips the automation policy to production and Caddy obtains a real
leaf. A DNS or firewall misconfiguration burns a cheap staging
failure instead of a prod rate-limit hit. Disable end-to-end with
[`acme_skip_staging: true`](../configuration.md) in `jacod.yaml`.

The controller lives at `internal/ingress/stagefirst/controller.go`
and is owned by the **raft leader's** daemon. The promotion loop
(`runStageFirst` in `internal/daemon/grpc/ingress.go`) self-gates on a
dynamic `node.IsLeader()` check every tick — the same self-gating
pattern as the scheduler/rebalance loops — so followers never stage,
self-check, clear the staging blob, or promote (issue #182). On leader
change the new leader picks up in-flight domains from replicated raft
state on its first leader tick. On every ~10 s tick (and on every
Routes / CertBlobs event) the leader's controller walks each
`tls: auto` domain:

1. **Not yet staged, no prod cert in raft** → add to the `staging`
   set. Next rebuild renders the domain's automation policy with the
   staging CA URL. Caddy obtains a staging leaf and stores it via
   the custom CertMagic storage (raft + on-disk fallback).
2. **Already staged, staging chain visible in storage** → run
   `SelfCheck`. On pass, log `staging self-check passed; promoting
   to prod`, fire the `ClearStagingCert` hook (see below), call
   `OnPromote`, mark the domain as **pending prod** with a
   `PendingProdWindow = 5 * time.Minute` deadline, drop it from
   the staging set so the next rebuild flips its policy to prod.
3. **Pending prod** → if `prodCertIssued(domain)` returns true the
   marker clears (Caddy landed the prod cert; `OnProdIssued` fires
   to record a `CERTIFICATE_ISSUED(prod)` audit event). If the
   deadline expires without prod landing, the controller increments
   the consecutive-failure counter, enters a **prod-issuance backoff**
   window (15 m on first failure, doubling to 1 h cap; see below),
   and fires `OnProdFail` to record a
   `CERTIFICATE_FAILED(prod, failure_class=rate_limit)` audit event
   (issue #189). The domain may re-stage only after that window
   expires.

The pending-prod window (issue #154) was added in v0.3.3 to break a
10 s flip-flop loop: pre-fix, the same-tick decision "domain not in
staging AND no prod cert in raft → stage it" fired the moment after
a promote, before Caddy could complete its prod ACME order, which
re-staged the domain, flipped the policy back to staging-CA, and
forced Caddy to abandon the in-flight prod issuance — repeating
indefinitely. The window holds the domain out of the re-stage
decision long enough for a real prod issuance to complete.

#### Challenge method: HTTP-01 only (TLS-ALPN-01 disabled)

The rendered Caddy automation policy **explicitly disables TLS-ALPN-01**
(`challenges.tls-alpn.disabled: true`) and keeps only HTTP-01 enabled
(issue #189). Behind an L4 / TCP-passthrough load-balancer that fans
`:443` across every node, TLS-ALPN-01 cannot work: its key-auth
material lives only on the order-initiating node
(`distributed=false` in CertMagic). Let's Encrypt's multi-perspective
validation opens several TLS connections that land on *different*
backends, none of which can answer → `remote error: tls: internal
error` → challenge fails.

HTTP-01 is not affected by the L4 fan-out — **provided** the challenge
key-auth is served CA-agnostically. This is the deeper half of issue
\#189:

##### Distributed challenge serving (the `jaco_acme_challenge` handler)

CertMagic's built-in distributed HTTP-01 solver keys its challenge-token
storage **by issuer/CA prefix**
(`<ca-prefix>/challenge_tokens/<domain>.json`). Behind an L4 LB a node
that renders a *different* CA policy than the order-initiating node — for
example a follower rendering prod during the leader's staging window
(issue #182), or any node mid-promotion — reads the challenge token under
the wrong CA prefix, gets `ErrNotExist`, and answers `404`. Let's
Encrypt's multi-perspective validation lands on those nodes and the
authorization deadlocks. (This is why issuance still failed behind the LB
even after TLS-ALPN-01 was disabled.)

JACO closes the gap with a **CA-agnostic, token-keyed** republish path:

1. **Storage tap** — `JacoStorage.Store` invokes a publisher hook for any
   key containing `/challenge_tokens/`. The hook parses the CertMagic
   `acme.Challenge` blob (`ParseHTTP01Blob`, HTTP-01 only) and calls
   `challenge.PublishToken`, which writes the `<token → keyAuth>` pair to
   raft's `ChallengeToken` set — keyed by the **token**, not by any CA.
2. **`jaco_acme_challenge` Caddy handler** — a terminal handler registered
   as `http.handlers.jaco_acme_challenge` and prepended to the `:80`
   routes (ahead of the HTTP→HTTPS redirect) for every
   `/.well-known/acme-challenge/*` request whenever `tls: auto` domains
   exist. It looks the token up in the replicated `ChallengeToken` set and
   serves the key-auth, or `404`s on a miss.

Because the token is replicated through raft and served by token (not CA
prefix), **any** node answers the challenge regardless of which CA policy
it is currently rendering. `PublishToken` is deliberately **not** audited
(CertMagic writes one challenge blob per order attempt; auditing each
would spam the log) — the single `CERTIFICATE_ISSUED` audit pair stays on
the issuance lifecycle.

##### Leader prod-race convergence (issue #189)

Once challenges are distributed, a follower rendering the prod CA can win
the prod ACME order outright while the leader is still in that domain's
staging window. The promotion controller therefore checks, on every
`Reconcile` pass, whether a prod cert already exists for a domain it is
still staging: if so it drops the domain from the staging set, clears any
pending/backoff bookkeeping, and fires `OnProdIssued` once (emitting the
`CERTIFICATE_ISSUED(prod)` audit) so the node converges to rendering and
serving prod instead of waiting forever for a staging cert that the race
made moot.

#### Prod-issuance exponential backoff (issue #189)

Without backoff, a failed prod order causes:
`PendingProdWindow expires → re-stage → re-promote → fresh prod order`
every ~5–6 min. Each new order is HTTP 429-rejected by Let's Encrypt,
which **extends** the failed-auth rate-limit window — the cluster
self-sustains the limit and it never resets.

The controller backs off per domain:

| Consecutive failures | Backoff window |
| --- | --- |
| 1 | 15 min |
| 2 | 30 min |
| 3+ | 60 min (cap) |

The counter resets to zero when a prod cert successfully lands in raft,
so a later renewal failure restarts the schedule at 15 min.

> **Note:** Let's Encrypt's `Retry-After` header is not observable
> here — the controller sees only "prod cert in raft: yes/no".
> Exponential-capped backoff is a conservative approximation.
> Backoff state is **issuing-node-local** (in-memory, not
> raft-replicated). A leader failover restarts the counter on the new
> leader; the new leader will retry within 5 min (first PendingProdWindow
> expiry triggers the 15 min backoff), then back off normally.
> Documented as a v1 limitation.

### Leader-gating and follower serving (issue #182)

Only the leader runs the promotion controller — but **every** node
must serve TLS, including during the transient staging window. So the
staging-vs-prod automation policy each node renders is derived from
**replicated** state, not from the leader's in-memory controller:

- `stagingDomainsFromState` (`internal/daemon/grpc/ingress.go`) returns
  the `tls: auto` domains that have a staging cert blob but no prod
  cert blob in `state.CertBlobs` — i.e. the domains currently in their
  staging window, cluster-wide.
- The config builder's staging set
  (`stagingDomainsForBuilder`) unions that replicated-state set with
  the controller's in-flight in-memory set **only on the leader** (the
  leader needs the in-memory entry to render the staging policy for a
  brand-new domain *before* any staging blob has landed in raft).
  Followers use the replicated-state set alone, so they render the
  staging policy and serve the replicated staging leaf during the
  window, then flip to prod when the promotion replicates.
- The rebuild reloader subscribes to `CertBlobs`, so a follower
  re-renders the moment a promotion replicates (staging blob removed,
  prod blob added).

Because Caddy's cert cache outlives `caddy.Load` (see
*Forcing fresh prod issuance* below), a follower that served the
staging leaf during the window would keep serving it from cache after
the prod cert lands. `runStageFirst` therefore runs a per-node
cache-reconcile pass on every tick that, on followers, calls
`cachepoke.EvictManaged(domain)` exactly once when a domain leaves the
staging-derived set and a prod blob is present. The leader does not
need this pass — it evicts precisely via `ClearStagingCert` at promote
time.

Before this gating (the bug #182 fixed), every node ran the promotion:
a follower would `ClearStagingCert` the cluster's staging blob from
raft, evict only its own cache, flip its own policy to prod, and then
fail to obtain a prod cert because the single-flight issue-lock is held
by the leader — so only the leader served TLS and HTTPS through a load
balancer was flaky.

### Forcing fresh prod issuance on promote

Flipping the automation policy's CA URL is by itself insufficient to
make Caddy obtain a fresh prod cert: the staging leaf remains valid
for ~90 days, certmagic's maintainer treats it as fine, and Caddy
keeps serving it. JACO's promote path explicitly clears both the
staging cert's persistence AND its in-process cache so the next TLS
handshake misses every layer and triggers obtain:

- **`ClearStagingCert` hook** (issue #158, v0.3.4) — wired in
  `internal/daemon/grpc/server.go` to call
  `clearStagingCertBlobs`, which deletes every staging-keyed
  `.crt` / `.key` / `.json` blob for the domain from the custom
  CertMagic storage. This catches both the raft state and the
  on-disk fallback cache, so a daemon restart-after-promote also
  lands a prod cert.
- **`cachepoke.EvictManaged`** (issue #163, v0.3.5,
  `internal/ingress/cachepoke/cachepoke.go`) — same closure also
  drops the matching managed cert from caddy v2's package-private
  `caddytls.certCache` singleton. The package uses `go:linkname` to
  reach the symbol; bumping `caddy/v2` in `go.mod` MUST sanity-check
  `internal/ingress/cachepoke` still compiles. The eviction calls
  `certmagic.Cache.RemoveManaged([]SubjectIssuer{{Subject: domain}})`
  with an empty `IssuerKey`, which per
  `certmagic@v0.25.3/cache.go:411` matches all managed certs for
  the subject regardless of issuer.

With both layers cleared, Caddy's next handshake for the domain
misses the cache, looks at storage under the now-prod-CA-namespaced
key, finds nothing, and CertMagic's manager starts the prod ACME
order. End-to-end test on a fresh 3-node cluster shows the served
cert flipping from `(STAGING) …` to a real LE prod intermediate
within seconds of the first post-promote handshake.

### Per-domain audit events

The controller emits typed audit events via the `storageApply` shim
(NOT the raw `apply` Applier — issue #146 — so a follower's emit
forwards to the leader and lands once cluster-wide):

- `CERTIFICATE_ISSUED(env: staging)` on `OnPromote` — "the staging
  dry-run passed for this domain."
- `CERTIFICATE_ISSUED(env: prod)` on `OnProdIssued` — "Caddy
  successfully obtained a prod cert against the now-prod policy"
  (issue #147; before v0.3.4 the env was hardcoded to `staging`
  and `jaco status` reported `staging` forever even after a real
  prod cert landed).
- `CERTIFICATE_FAILED{stage_failed_at: staging}` on `OnStageFail`
  — the staging chain landed but failed `SelfCheck`. The controller
  records a 1 h backoff before re-staging the same domain.

`jaco status` reads `ENVIRONMENT` directly from the cert blob key
(`internal/controlplane/grpc/status.go`): the key path embeds the
CA directory URL, so a blob under `acme-v02.api.letsencrypt.org-directory`
renders as `prod` regardless of the audit-event sequence.

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

All write paths (`Store`, `Delete`, `Lock`, `Unlock`) go through an
apply-or-forward shim: a follower's raft Apply returns
`hraft.ErrNotLeader`, which the shim catches and re-issues as an
`Internal.Submit` RPC to the leader's gRPC address (resolved from
`state.Nodes`). Cluster-wide single-acquisition is preserved by the
existing `CertLock` FSM rules (`LockTTL`, lessee identity). Before this
forwarding (issue #112), Caddy's tls maintenance loop would log
`node is not the leader - storage is probably misconfigured` every
~10 minutes on every non-leader node.

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
