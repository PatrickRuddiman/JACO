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

- **Deployments** — `DEPLOYMENT, REVISION, PREVIOUS, STATUS`. Status
  is one of `PENDING`, `ACTIVE` (see
  [Status and errors](../concepts/status-and-errors.md)).
- **Replicas** — `REPLICA_ID, STATE, HOST, CONTAINER_ID,
  LAST_HEALTH_AT`. State is from the closed
  `pending | pulling | running | degraded | updating | failed | stopped`
  enum.
- **Routes** — `DOMAIN, DEPLOYMENT, SERVICE, PORT, TLS`. TLS is
  `auto` or `off`.
- **Certs** — `DOMAIN, ENVIRONMENT, NOT_AFTER, LAST_RENEWAL_AT`. Only
  rendered when at least one managed cert exists.

With `-w`, the initial snapshot prints, then the CLI opens a
`Watch.Subscribe` stream filtered to `deployments`, `replicas_observed`,
and `routes`. Each event prints a `---` separator followed by a fresh
snapshot. Resync events trigger an idempotent re-fetch.

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

- [`jaco apply`](apply.md)
- [`jaco logs`](logs.md)
- [Status and errors](../concepts/status-and-errors.md)
- [Scheduling](../concepts/scheduling.md)
