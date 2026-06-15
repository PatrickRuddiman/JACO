---
sources:
  - cmd/jaco/get.go
  - internal/controlplane/grpc/get.go
  - internal/controlplane/grpc/status.go
  - internal/controlplane/state/
---

# `jaco get`

Read the current in-raft spec for a deployment, replica, or route — the
state JACO **actually stored**, not just the fixed projection `jaco status`
shows (issue #175). Built for incident response: dump a deployment's
`compose`/`jaco` spec, a replica's `depends_on` gates, or a domain's path
matches without holding the operator's source compose file.

## Synopsis

```
jaco get deployments
jaco get deployment <name>
jaco get replicas [--deployment <name>] [--service <name>]
jaco get replica <id>
jaco get routes [--domain <domain>]
jaco get route <domain>
```

Every subcommand accepts the standard transport flags and honors the global
`-o table|json|yaml`:

```
[--server <host:port> --token <op>] [--ca-cert <path>] [--socket <path>]
```

## Subcommands

| command                          | purpose                                                              |
|----------------------------------|----------------------------------------------------------------------|
| `jaco get deployments`           | list deployments: name, applied/previous revision, status, services  |
| `jaco get deployment <name>`     | full spec for one deployment incl. the stored `jaco.yaml` + `compose.yaml` |
| `jaco get replicas`              | list replicas: id, deployment, service, state, host, restart count, image |
| `jaco get replica <id>`          | one replica's spec, state, restart count, and resolved `depends_on` gates |
| `jaco get routes`                | list ingress routes **including the path-prefix column**             |
| `jaco get route <domain>`        | every ingress entry for one domain, in path-match order, with upstream readiness |

`get replicas` accepts `--deployment` and `--service` filters; `get routes`
accepts `--domain`.

## Auth

Operator token (TCP, `--server`) or unix-socket trust (local). These are
read-only RPCs that read local state — no raft leader is required, so they
can be served by any node.

## Output

- **table** (default) — human columns; enum values UPPERCASE.
- **json / yaml** — structured; enum values lowercase `snake_case` (e.g.
  `state: pending`), matching the convention used by `jaco status`.
  `jaco get deployment <name> -o yaml` embeds the raw `jaco_yaml` and
  `compose_yaml` as strings so the dump is self-contained.

### Replica detail

`jaco get replica <id>` joins three stores so a single PENDING replica is
self-diagnosing:

- desired spec — `deployment`, `service`, `index`, `image`, `revision`
  (the scheduling raft index), placement `host`;
- latest observation — `state`, `container_id`, `code`/`message` (the last
  health/transition reason, e.g. a pull error), `started_at`,
  `last_health_at`, plus the raw `details` map;
- restart counter — `restart_count` and `last_attempt_at`;
- `depends_on` — each compose-declared dependency with its `condition`, the
  dependency service's current aggregate `state`, and whether the gate is
  `satisfied` right now (same semantics the reconciler enforces).

## Examples

```console
$ jaco get deployment app -o yaml
name: app
applied_revision: 11
previous_revision: 10
status: active
services:
  - name: api
    replicas: 2
    placement: spread
  - name: web
    replicas: 3
    placement: spread
jaco_yaml: |
  deployment: app
  ...
compose_yaml: |
  services:
    web:
      image: nginx:1.27
      depends_on: [api]
  ...

$ jaco get replica app-web-0 -o yaml
id: app-web-0
deployment: app
service: web
state: pending
restart_count: 3
depends_on:
  - service: api
    condition: service_started
    state: pending
    satisfied: false

$ jaco get route example.com
Routes for example.com:
PATH  SERVICE  PORT  TLS   STRIP  FALLBACK  READY
/api  api      8080  auto  yes    no        2/2
      web      80    auto  no     yes       3/3
```

The catch-all row has an empty `PATH` and `FALLBACK yes`; `READY` is the
`ready/total` healthy-upstream count. See [`jaco get route`](get-route.md)
for the full column reference and the `-o json` shape.

## Not yet supported

Both require new raft-replicated history storage and are deferred to a
follow-up:

- `jaco get deployment <name> --revision N` — historical revisions. The
  deployment store keeps only the currently-applied revision; older
  revisions are not retained.
- replica `state_history` — the last N state transitions with reasons.
  Only the latest observation is stored today.

## See also

- [`jaco status`](status.md) — the fixed snapshot projection.
- [`jaco audit`](audit.md) — who applied what, when.
