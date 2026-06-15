---
sources:
  - cmd/jaco/status.go
  - internal/controlplane/grpc/watch.go
  - internal/controlplane/state/
---

# `jaco status`

Print a snapshot of cluster deployments, replicas, routes, and managed
TLS certs. With `-w`, re-render on every state change.

## Synopsis

```
jaco status [deployment[/service]] [-w]
            [--server <host:port> --token <op>]
            [--ca-cert <path>] [--socket <path>]
```

## Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (with `--server`)       |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                                |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `-w, --watch`         | `false`                       | re-render on every state change (Ctrl-C exits) |

Positional argument is `dep` or `dep/svc` to scope the snapshot to one
deployment or one service within it.

## Auth

Operator token (TCP) or unix-socket trust (local).

## Behavior

Renders up to four tables, in order:

- **Deployments** — `DEPLOYMENT, REVISION, PREVIOUS, STATUS, DETAILS`.
  Status is one of `PENDING`, `ACTIVE` (see
  [Status and errors](../concepts/status-and-errors.md)). `DETAILS`
  carries the scheduler's `reason` when a deployment is `PENDING` (e.g. an
  unschedulable `placement: hosts` pin); blank for `ACTIVE`.
- **Replicas** — `REPLICA_ID, STATE, HOST, CONTAINER_ID,
  LAST_HEALTH_AT, REASON`. State is from the closed
  `pending | pulling | running | degraded | updating | failed | stopped`
  enum. `REASON` shows the observed failure `code` (and message) for a
  replica that isn't healthy — e.g. `container_exited (exit 1)` or
  `image_pull_failed: manifest unknown`; blank for a healthy `running`
  replica. `HOST` falls back to the replica's scheduled host when the
  runtime reported the observation without one, so a `failed` replica
  still shows where it ran.
- **Routes** — `DOMAIN, PATH, DEPLOYMENT, SERVICE, PORT, TLS`. `PATH` is
  the URL path prefix the route matches; empty means catch-all. Routes that
  share a `DOMAIN` but differ by `PATH` are path-scoped (Caddy tries the
  longest prefix first, falling through to the catch-all). TLS is
  `auto` or `off`.
- **Certs** — `DOMAIN, ENVIRONMENT, NOT_AFTER, LAST_RENEWAL_AT`. Only
  rendered when at least one managed cert exists.

With `-w`, the initial snapshot prints, then the CLI opens a
`Watch.Subscribe` stream filtered to `deployments`, `replicas_observed`,
and `routes`. Each event prints a `---` separator followed by a fresh
snapshot. Resync events trigger an idempotent re-fetch.

## Output formats

`-o json` and `-o yaml` emit a structured snapshot instead of the tables.
Enum fields use **lowercase `snake_case`** values — the table view's
UPPERCASE is for human scanning only; scripts should match the lowercase
form below (replica `state`, deployment `status`):

```json
{
  "deployments": [
    { "name": "mydeploy", "applied_revision": 8, "previous_revision": 7, "status": "active" },
    { "name": "website", "applied_revision": 6, "previous_revision": 5, "status": "pending",
      "status_details": { "reason": "service \"mirror\" pins host \"ghost\" which is not a cluster member" } }
  ],
  "replicas": [
    { "id": "mydeploy-web-0", "state": "running", "host": "node-a", "container_id": "c1", "last_health_at": "2026-06-01T12:00:00Z" },
    { "id": "website-web-0", "state": "failed", "host": "node-a", "container_id": "c9", "last_health_at": "2026-06-01T12:00:00Z",
      "code": "container_exited", "message": "container exited", "details": { "exit_code": "1" } }
  ],
  "routes": [
    { "domain": "web.example.com", "deployment": "mydeploy", "service": "web", "port": 80, "tls": "auto" }
  ],
  "certs": [
    { "domain": "web.example.com", "environment": "prod", "not_after": "2026-08-01T00:00:00Z", "last_renewal_at": "2026-06-02T00:00:00Z" }
  ]
}
```

- `status` is one of `pending`, `active`.
- `state` is one of
  `pending | pulling | running | degraded | updating | failed | stopped`.
- `tls` is `auto` or `off`.
- `status_details` (deployment) carries the scheduler `reason` when the
  status is `pending`; omitted otherwise.
- replica `code` / `message` / `details` mirror the table's `REASON`
  column (`details.exit_code` for an exited container, `details.reason`
  for a pull failure); all three are omitted for a healthy replica.
- `certs` is omitted when no managed cert exists. Timestamp fields are
  RFC3339 UTC and omitted when unset.
- `-o yaml` carries the same fields and casing.

With `-w`, json output is a stream of concatenated JSON snapshots (a valid
`jq` input); yaml output separates snapshots with `---` document breaks.

Example — poll for rollout convergence with `jq`:

```sh
jaco status mydeploy -o json | jq -e '.replicas | all(.state == "running")'
```

## Exit codes

- `0` — snapshot rendered (or watch loop exited cleanly).
- `1` — auth or transport error.

## Examples

Whole-cluster snapshot:

```sh
jaco status --server $LEADER
```

Single-deployment, single-service, follow:

```sh
jaco status --server $LEADER hello/web -w
```

Local-socket snapshot during cluster bring-up:

```sh
sudo jaco status hello
```

## See also

- [`jaco get route`](get-route.md)
- [`jaco apply`](apply.md)
- [`jaco logs`](logs.md)
- [Status and errors](../concepts/status-and-errors.md)
- [Scheduling](../concepts/scheduling.md)
