---
sources:
  - internal/controlplane/grpc/jaco_spec.go
  - proto/jaco/v1/entities.proto
---

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
environment: <path>          # optional; path to a KEY=value env file
routes:                      # required, list — the point of the file
  - domain: <fqdn>
    service: <service>
    port: <int>
    tls: auto | off
    path: <url-prefix>
    strip_path: <bool>
services:                    # optional, list of overrides
  - name: <service>
    replicas: <int >= 0>
    placement: spread | pack | hosts | global
    hosts: [host-a, host-b]
    networks: [net-a, net-b]
acme: on | off               # optional; on by default
acme_email: ops@example.com  # optional; per-stack ACME contact (#102)
```

Only `deployment`, `environment`, `routes`, `services`, `acme`, and
`acme_email` are accepted at the top level. Anything else is rejected.

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
- **`global`** (daemonset) — exactly one replica per ready node. The
  scheduler derives the count from the cluster's node set, growing and
  shrinking automatically as nodes join or leave. `replicas:` is
  **mutually exclusive** with `global`: declaring both is rejected at
  apply with `service "x" uses placement=global; remove replicas`.
  `hosts:` is also ignored. Replica ids are keyed by hostname (not a
  positional index), so a surviving node's replica is unchanged when a
  peer departs.

`placement` and `hosts` interact: `placement: hosts` requires `hosts`;
`placement: spread | pack | global` ignores `hosts`. The closed enum is
enforced by the proto `ServiceSpec.PlacementMode`
([`proto/jaco/v1/entities.proto`](../../proto/jaco/v1/entities.proto)).

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

## `acme_email`

Per-stack ACME (Let's Encrypt) contact address. Each stack with a
distinct non-empty `acme_email:` gets its **own Caddy automation
policy and its own ACME account**, so renewal notifications reach the
stack's owner instead of one global ops inbox.

- Empty (default) → fall back to the cluster-wide `acme_email` in
  `jacod.yaml`.
- Two stacks with the same value share a single ACME account /
  automation policy.
- Validation is syntactic only (`net/mail.ParseAddress`) and only
  runs when ACME is on; under `acme: off` the field is accepted but
  unused.
- **Changing a stack's `acme_email`** creates a fresh ACME account
  registration on the next issuance / renewal; existing valid certs
  keep serving until renewal.

## `environment`

Optional path to an env-style file that fills the **compose-spec
`${VAR}` interpolation environment** for the adjacent compose file.
Resolved CLIENT-SIDE by `jaco apply`: the file is read, parsed, and
its values are substituted into every scalar of the compose document
before the bytes cross the wire to the daemon.

```yaml
# jaco.yaml
deployment: myapp
environment: .env
routes: ...
services: ...
```

> **Naming clash callout.** Compose has a per-service `environment:`
> key whose value is a **map** of `KEY: value` pairs forwarded into a
> single container. The jaco.yaml top-level `environment:` is a
> different field: its value is a **path string** and it governs
> `${VAR}` substitution across the whole compose document. The
> namespaces never collide in a real manifest (one lives on the
> jaco.yaml root, the other under each compose service), but the same
> keyword in two adjacent files is intentional — operators
> consistently asked for the top-level field to be called
> `environment:`.

### File format

The referenced file is the standard docker-compose `.env` format
(implemented by `compose-go/dotenv`):

- One `KEY=value` per line. Whitespace around `=` is trimmed.
- `#` starts a comment to end-of-line. Blank lines are ignored.
- Quoted values (`KEY="value with spaces"`, `KEY='literal'`) honor
  the usual unquote rules; unquoted values run to end-of-line.
- Back-references resolve against earlier keys in the same file:
  `BASE=foo` then `DERIVED=${BASE}.bar` produces
  `DERIVED=foo.bar`. There is **no process-environment fallback** —
  see [Semantics](#semantics) below.

### Path resolution

Relative paths resolve against the jaco.yaml file's directory (same
convention compose's service-level `env_file:` uses against the
compose file). An absolute path is honored as-is.

A missing or unreadable file fails the apply with a clear CLI error
naming the offending path — the operator's responsibility to have it
present at apply time.

### Semantics

- **Interpolation only.** Values from the file are NOT auto-injected
  into every service's compose `environment:` block. A container
  sees a value only via an explicit `${VAR}` reference somewhere in
  the compose document.
- **No process-environment passthrough.** The interpolation map is
  the env file contents, period. `os.Environ()` does not
  participate, keeping manifests explicit and reproducible across
  operators / hosts.
- **Coexists with service `env_file:`.** Per-compose-spec precedence:
  an explicit `environment:` value on a service (with `${VAR}`
  resolved against the jaco.yaml file) wins over a service-level
  `env_file:` entry, which in turn wins over a default supplied via
  `${VAR:-default}` from the same source. See
  [`compose.md` → env_file resolution](compose.md#env_file-resolution).
- **At-rest posture.** Resolved values ride the wire baked into the
  compose YAML stored on the per-deployment record raft already
  replicates and snapshots — no separate "env" entity, no separate
  rotation. See
  [Auth & tokens → at-rest posture](../concepts/auth-and-tokens.md#registry-credentials)
  for the trust boundary that already governs `compose_yaml`.
- **Omitting the field** is the no-op default and preserves every
  existing manifest byte-for-byte through the CLI.

### Example

```yaml
# jaco.yaml
deployment: myapp
environment: .env
routes:
  - domain: api.example.com
    service: api
    port: 8080
    tls: auto
```

```yaml
# compose.yml — ${VAR} interpolates from jaco.yaml's .env file
services:
  api:
    image: ${REGISTRY:-docker.io}/myorg/api:1
    environment:
      DB_URL: ${DB_URL}
      REGION: ${AWS_REGION:-us-east-1}
```

```
# .env (next to jaco.yaml)
REGISTRY=ghcr.io
DB_URL=postgres://db.internal/myapp
# AWS_REGION not set; the ${AWS_REGION:-us-east-1} default applies
```

## Precedence

For every compose service, the apply path computes a final
ServiceSpec by merging compose defaults with the jaco entry (if any)
in this order:

| field        | source (highest priority first)                            |
|--------------|------------------------------------------------------------|
| `replicas`   | jaco `replicas:` if set → compose `deploy.replicas` → `1`  |
| `placement`  | jaco `placement:` if set → `spread`                        |
| `hosts`      | jaco `hosts:` if set → empty (only used under `placement: hosts`) |
| `networks`   | jaco `networks:` if non-empty → compose service `networks:` keys (sorted) |
| `acme_email` | jaco `acme_email:` if set → cluster-wide `jacod.yaml`'s `acme_email` |

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
