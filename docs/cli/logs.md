---
sources:
  - cmd/jaco/logs.go
  - internal/runtime/logs/
  - internal/daemon/grpc/
---

# `jaco logs`

Stream container logs from every replica of a service from any node.
The entry node opens peer RPCs to every host running a target replica
and merges the streams by arrival.

## Synopsis

```
jaco logs <deployment>/<service> [-f] [--since <dur>]
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
| `-f, --follow`        | `false`                       | stream new lines as they arrive               |
| `--since <dur>`       | `5m`                          | only lines newer than this duration           |

`--since` accepts any Go `time.ParseDuration` value (`30s`, `5m`, `1h`).

The positional argument is exactly `<deployment>/<service>`. Anything
else fails with `expected <deployment>/<service>, got "…"`.

## Auth

Operator token (TCP) or unix-socket trust (local). Peer fanout uses
mutually authenticated TLS and forwards the original operator token;
the receiving node validates it again. A peer certificate by itself
does not grant arbitrary workload-log access.
Authority is rechecked before each peer log line, so applied token
revocation, member removal, or CA replacement stops further disclosure.
A failed peer stream cancels the remaining fanout and returns an error,
rather than silently reporting an incomplete stream as successful.

The current leader can fan out logs for its local unix-socket operator
without a bearer token. On a follower, tokenless unix-socket requests
can read locally hosted replicas only. If the request also needs remote
replicas, it fails before emitting any lines with guidance to use an
operator bearer over `--server`, or the leader's local socket. This
intentional restriction prevents every cluster member from acquiring
unrestricted cross-host log-reading authority.

## Behavior

Each line is rendered as:

```
[<replica-id>@<host>] <line>
```

Lines from a single replica preserve their original order; lines across
replicas are interleaved by arrival time at the streaming node (no
global re-ordering or buffering).

Without `--follow`, the call has a 60-second deadline and exits once
the historical window is drained. With `--follow`, the deadline is
removed and the stream runs until the operator hits Ctrl-C.

## Exit codes

- `0` — stream completed (or cancelled with Ctrl-C).
- `1` — `unknown_service`, auth, or transport error.

## Examples

Tail one service's last hour across every replica:

```sh
jaco logs --server $LEADER hello/web --since 1h
```

Follow live:

```sh
jaco logs --server $LEADER hello/web --follow
```

On the leader, no token needed for cluster-wide fanout (on a follower,
this tokenless form is limited to locally hosted replicas):

```sh
sudo jaco logs hello/web -f
```

## See also

- [`jaco status`](status.md)
- [`jaco apply`](apply.md)
- [Architecture](../concepts/architecture.md)
