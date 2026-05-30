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

A deployment is a `jaco.yaml` + compose pair. The compose file declares
service shapes (image, environment, healthcheck, networks); the
`jaco.yaml` overlay declares cluster-level concerns (how many replicas,
which hosts, public ingress).

## Top-level shape

```yaml
deployment: <name>           # required, string
services:                    # required, list
  - name: <service>
    replicas: <int >= 0>
    placement: spread | pack | hosts | global
    hosts: [host-a, host-b]
    networks: [net-a, net-b]
routes:                      # optional, list
  - domain: <fqdn>
    service: <service>
    port: <int>
    tls: auto | off
    path: <url-prefix>
    strip_path: <bool>
```

Only `deployment`, `services`, and `routes` are accepted at the top
level. Anything else is rejected.

## `deployment`

The deployment name, used as the raft key and as a prefix for replica
ids (`<deployment>-<service>-<index>`). Must be unique across the
cluster.

## `services[*]`

| field        | type                          | required | default              |
|--------------|-------------------------------|----------|----------------------|
| `name`       | string                        | yes      | —                    |
| `replicas`   | int ≥ 0                       | yes      | —                    |
| `placement`  | `spread \| pack \| hosts \| global` | no | `spread`     |
| `hosts`      | list of hostnames             | when `placement: hosts` | — |
| `networks`   | list of compose network names | no       | `[_default]`         |

`name` must match a key in the compose file. The cross-check is
performed at apply time; mismatches reject with `unknown service: <name>;
declared compose services: [...]`.

`replicas: 0` is legal — the deployment, its routes, and (for
`tls: auto` domains) the cert lifecycle stay provisioned, with no
container running. Setting `replicas` back up brings the service back
online within the apply-to-steady-state window without re-issuing
certs.

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
- **`global`** (daemonset) — exactly one replica per ready node.
  `replicas:` is **ignored** under `global`; the effective count is
  the number of eligible nodes, and grows or shrinks automatically as
  nodes join or leave. `hosts:` is also ignored. Replica ids are keyed
  by hostname (not a positional index), so a surviving node's replica
  is unchanged when a peer departs.

`placement` and `hosts` interact: `placement: hosts` requires `hosts`;
`placement: spread | pack | global` ignores `hosts`. The closed enum
is enforced by the proto `ServiceSpec.PlacementMode`
([`proto/jaco/v1/entities.proto`](../../proto/jaco/v1/entities.proto)).

### `networks`

Names of compose-level networks the service attaches to. Each must
match a key in the compose file's top-level `networks:` block; the
implicit `_default` network is always considered declared.

A service with no `networks` field attaches to the per-deployment
`_default` network. Two services share a network iff both declare it.
See [Networking](../concepts/networking.md) and
[Isolation](../concepts/isolation.md).

## `routes[*]`

Optional list. Each entry declares one public HTTP(S) route serviced
by the embedded Caddy on every cluster node.

| field        | type                  | required | default | meaning                                            |
|--------------|-----------------------|----------|---------|----------------------------------------------------|
| `domain`     | FQDN                  | yes      | —       | host header to match                               |
| `service`    | service name          | yes      | —       | upstream service within this deployment            |
| `port`       | int                   | yes      | —       | container port to dial                             |
| `tls`        | `auto \| off`         | no       | `auto`  | ACME-issued cert (`auto`) or plaintext (`off`)     |
| `path`       | URL prefix            | no       | `""`    | longest-prefix-first; default is catch-all         |
| `strip_path` | bool                  | no       | `false` | strip `path` prefix from the request URI upstream  |

- `tls: auto` triggers ACME issuance for `domain`. JACO retries with
  exponential backoff capped at 1 hour on failure; while pending,
  plaintext HTTP for the domain remains active.
- `tls: off` declines TLS for the domain — HTTP only, no cert is
  obtained.
- `path` allows two routes for the same `domain` provided their paths
  differ. Caddy is fed routes longest-prefix-first so the more
  specific path wins.
- `strip_path: true` rewrites the request URI to remove `path` before
  proxying to the upstream — e.g. with `path: /api` and `strip_path:
  true`, an inbound `GET /api/foo` reaches the container as
  `GET /foo`. Has no effect when `path` is empty. Default `false`
  forwards the full URI byte-for-byte (the legacy behavior).

Raw-TCP ingress is not declared in `routes`. It is implicit: any
compose service with `ports: ["H:C"]` registers a cluster-wide TCP
listener on host port `H` forwarded to container port `C`. Host ports
80 and 443 are reserved for Caddy and rejected at apply.

## Cross-file validation

On apply, the admission path validates both files together:

- Every `services[*].name` in jaco.yaml must match a compose service.
- Every network referenced by a compose service must appear under the
  top-level `networks:` block.
- Every `hosts[*]` must be a known cluster member.
- `len(eligible_hosts) >= replicas` is *not* enforced at apply (the
  scheduler reports `pending` instead) — but
  `placement: hosts` with too few `hosts` to support `replicas` is
  rejected upfront with
  `cannot place N replicas on M pinned hosts`.

## Minimal example

```yaml
deployment: sample
services:
  - name: web
    replicas: 2
routes:
  - domain: web.example.com
    service: web
    port: 80
    tls: auto
```

That's the canonical sample shipped in `cmd/jaco/testdata/sample.jaco.yaml`.

## See also

- [Supported compose fields](compose.md)
- [Examples](examples.md)
- [Scheduling](../concepts/scheduling.md)
- [Networking](../concepts/networking.md)
