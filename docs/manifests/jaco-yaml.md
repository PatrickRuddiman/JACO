# `jaco.yaml` schema

The overlay manifest is a small, **closed** schema. Every field is
listed here; any other key fails the apply with `validation_failed` and
the offending field in the error details. No state changes on
rejection.

A deployment is a `jaco.yaml` + compose pair. **Compose is the source
of truth for the container set** — image, environment, healthcheck,
ports, networks. `jaco.yaml` declares the cluster-level concerns the
operator wants on top of that: which routes are public, and (when the
compose defaults aren't right) per-service overrides for replicas,
placement, host pinning, or network attachment.

`services:` is **optional**. A jaco.yaml that declares only routes is
valid — every compose service inherits sensible defaults (one replica,
placement `spread`, the compose-declared networks). When you do
declare a service entry, it is interpreted as an **override**: any
field you leave unset falls back to the compose default. See
[Precedence](#precedence) below.

## Top-level shape

```yaml
deployment: <name>           # required, string
routes:                      # optional, list — usually what you came for
  - domain: <fqdn>
    service: <service>
    port: <int>
    tls: auto | off
    path: <url-prefix>
services:                    # optional, list of overrides
  - name: <service>
    replicas: <int >= 0>
    placement: spread | pack | hosts | global
    hosts: [host-a, host-b]
    networks: [net-a, net-b]
acme: on | off               # optional; on by default
```

Only `deployment`, `routes`, `services`, and `acme` are accepted at
the top level. Anything else is rejected.

## `deployment`

The deployment name, used as the raft key and as a prefix for replica
ids (`<deployment>-<service>-<index>`). Must be unique across the
cluster.

## `routes[*]`

Each entry declares one public HTTP(S) route serviced by the embedded
Caddy on every cluster node.

| field      | type                  | required | default | meaning                                            |
|------------|-----------------------|----------|---------|----------------------------------------------------|
| `domain`   | FQDN                  | yes      | —       | host header to match                               |
| `service`  | service name          | yes      | —       | upstream service within this deployment            |
| `port`     | int                   | yes      | —       | container port to dial                             |
| `tls`      | `auto \| off`         | no       | `auto`  | ACME-issued cert (`auto`) or plaintext (`off`)     |
| `path`     | URL prefix            | no       | `""`    | longest-prefix-first; default is catch-all         |
| `strip_path` | bool                | no       | `false` | strip the matched path prefix before proxying      |

- `service` must match a service the compose file declares (with or
  without a corresponding `services[*]` override). A route that names
  a service neither compose nor jaco.yaml knows about is rejected at
  apply with `route ... references unknown service ...`.
- `tls: auto` triggers ACME issuance for `domain`. JACO retries with
  exponential backoff capped at 1 hour on failure; while pending,
  plaintext HTTP for the domain remains active.
- `tls: off` declines TLS for the domain — HTTP only, no cert is
  obtained.
- `path` allows two routes for the same `domain` provided their paths
  differ. Caddy is fed routes longest-prefix-first so the more
  specific path wins.
- A single domain MUST use one TLS mode across all its routes — mixing
  `tls: auto` and `tls: off` on the same domain is rejected with
  `route_tls_mixed` (Caddy can't half-redirect a host).

Raw-TCP ingress is not declared in `routes`. It is implicit: any
compose service with `ports: ["H:C"]` registers a cluster-wide TCP
listener on host port `H` forwarded to container port `C`. Host ports
80 and 443 are reserved for Caddy and rejected at apply.

## `services[*]`

Optional list of per-service overrides. Every field except `name` is
optional — anything left unset uses the compose default (see
[Precedence](#precedence)).

| field        | type                          | required | compose default                          |
|--------------|-------------------------------|----------|------------------------------------------|
| `name`       | string                        | yes      | —                                        |
| `replicas`   | int ≥ 0                       | no       | `deploy.replicas` if set, else `1`       |
| `placement`  | `spread \| pack \| hosts \| global` | no | `spread`                                 |
| `hosts`      | list of hostnames             | when `placement: hosts` | —                          |
| `networks`   | list of compose network names | no       | keys of the compose service's `networks:` |

`name` must match a service key in the compose file. A jaco entry
that names a service the compose file doesn't declare is rejected at
apply — there is nothing to override.

`replicas: 0` is legal — the deployment, its routes, and (for
`tls: auto` domains) the cert lifecycle stay provisioned, with no
container running. Setting `replicas` back up brings the service back
online within the apply-to-steady-state window without re-issuing
certs. Omitting `replicas:` entirely is different: it means "use the
compose default", which is either `deploy.replicas` from the compose
file or `1`.

### Placement modes

- **`spread`** (default) — replicas are placed across all healthy
  nodes. Replica `i` lands on
  `eligible[hash(deployment+service+i) mod len(eligible)]`. Stable
  across reconciles: the same input produces the same host, so leader
  failovers don't churn replicas.
- **`pack`** — replicas pile onto the lowest-loaded host first
  (lowest current replica count, any service). Ties broken by
  hostname.
- **`hosts`** — replicas are placed only on hosts in the `hosts`
  list. Requires a non-empty `hosts`. If `len(eligible) < replicas`,
  the apply succeeds but the deployment status becomes `pending` with
  details `{reason: cannot_satisfy_host_placement, missing: [...]}`
  visible in `jaco status` — no replicas are scheduled elsewhere.
- **`global`** — one replica per ready node (daemonset). The
  scheduler derives the count from the cluster's node set, so
  `replicas:` is **mutually exclusive** with `global`: declaring
  both is rejected at apply with
  `service "x" uses placement=global; remove replicas`. Use
  `placement: global` alone.

`placement` and `hosts` interact: `placement: hosts` requires `hosts`;
`placement: spread | pack | global` ignores `hosts`. The closed enum is
enforced by the proto `ServiceSpec.PlacementMode`.

### `networks`

Names of compose-level networks the service attaches to. Each must
match a key in the compose file's top-level `networks:` block; the
implicit `_default` network is always considered declared.

When the jaco entry omits `networks:`, the service inherits the
compose service's `networks:` keys (sorted alphabetically for
determinism). When the jaco entry sets `networks:`, the JACO list
wins outright — no per-element merge. A service whose compose entry
also declares no networks attaches to the per-deployment `_default`
network.

See [Networking](../concepts/networking.md) and
[Isolation](../concepts/isolation.md).

## `acme`

Deployment-level ACME switch. `acme: off` implicitly disables TLS on
every route that didn't set `tls:` explicitly — a convenience opt-out
for dev/internal deployments that don't want JACO racing the
operator to Let's Encrypt. An individual route may still opt back in
with an explicit `tls: auto`. Empty / `on` (default) leaves each
route's TLS decision to the route itself.

## Precedence

For every compose service, the apply path computes a final
ServiceSpec by merging compose defaults with the jaco entry (if any)
in this order:

| field      | source (highest priority first)                            |
|------------|------------------------------------------------------------|
| `replicas` | jaco `replicas:` if set → compose `deploy.replicas` → `1`  |
| `placement`| jaco `placement:` if set → `spread`                        |
| `hosts`    | jaco `hosts:` if set → empty (only used under `placement: hosts`) |
| `networks` | jaco `networks:` if non-empty → compose service `networks:` keys (sorted) |

Every compose service produces a ServiceSpec, even without a matching
jaco entry. The merged set is what the scheduler and the runtime see.

## Cross-file validation

On apply, the admission path validates both files together:

- Every `services[*].name` in jaco.yaml must match a compose service.
- Every `routes[*].service` must match the merged service set —
  compose-only services count.
- Every network referenced by a compose service must appear under the
  top-level `networks:` block.
- Every `hosts[*]` must be a known cluster member.
- `len(eligible_hosts) >= replicas` is *not* enforced at apply (the
  scheduler reports `pending` instead) — but
  `placement: hosts` with too few `hosts` to support `replicas` is
  rejected upfront with
  `cannot place N replicas on M pinned hosts`.

## Minimal examples

**Routes-only** (compose supplies the rest):

```yaml
deployment: sample
routes:
  - domain: web.example.com
    service: web
    port: 80
    tls: auto
```

**Routes plus a per-service override** (scale `web` to 3 replicas;
pin a db to one host):

```yaml
deployment: sample
routes:
  - domain: web.example.com
    service: web
    port: 80
    tls: auto
services:
  - name: web
    replicas: 3
  - name: db
    placement: hosts
    hosts: [storage-1]
```

## See also

- [Supported compose fields](compose.md)
- [Examples](examples.md)
- [Scheduling](../concepts/scheduling.md)
- [Networking](../concepts/networking.md)
