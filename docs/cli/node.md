---
sources:
  - cmd/jaco/node.go
  - internal/controlplane/grpc/cluster.go
  - internal/scheduler/drain/
---

# `jaco node`

Cluster membership management: mint join tokens, attach new nodes,
remove existing ones, list members.

## `jaco node issue-join-token`

### Synopsis

```
jaco node issue-join-token [--server <host:port> --token <op>] [--show-ca] [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (required with `--server`) |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM (used with `--server`)         |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `--show-ca`           | `false`                       | append the cluster CA PEM to the output       |

### Auth

Operator token (TCP path) or unix-socket trust (local path).

### Behavior

Mints a single-use, 24-hour-TTL join token. The hashed secret is stored
in raft as a `JoinToken{}` entity; the plaintext is printed once.
Output is the exact `jaco node join` invocation the operator will run
on the joining node. With `--show-ca`, the cluster CA PEM is appended
— write that to a file on the joining node and pass via `--ca-cert`.

### Exit codes

- `0` — token issued.
- `1` — auth failure or transport error.

### Examples

```sh
export JACO_TOKEN=<operator_token>
jaco node issue-join-token --server node-1:7000
# Join token issued. On the joining node, run:
#
#   sudo jaco node join --peer=node-1:7000 --token=<single-use>
#
# Token expires in 24h (single-use).
```

## `jaco node join`

### Synopsis

```
sudo jaco node join --peer <host:port> --token <single-use> [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                  |
|-----------------------|-------------------------------|------------------------------------------|
| `--peer <addr>`       | — (required)                  | leader or any cluster member's gRPC      |
| `--token <s>`         | `JACO_JOIN_TOKEN`             | single-use join token                    |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                  |

### Auth

Unix-socket only. The CLI calls `Cluster.Join` on the local daemon; the
daemon performs the cross-host raft + CSR exchange itself.

### Behavior

The local daemon generates a CSR, dials `--peer` over TLS, exchanges
the join token for a signed node cert + cluster CA + raft peer set,
persists everything under `$JACO_DATA_DIR/node/`, opens its raft node,
and joins the existing cluster. The join token is consumed (marked
`consumed_at` in raft) and cannot be reused.

### Exit codes

- `0` — node joined.
- `1` — bad token, network unreachable, or `cluster_already_initialized`.

### Examples

```sh
sudo jaco node join --peer node-1:7000 --token <single-use>
# Joined cluster.
```

## `jaco node remove`

### Synopsis

```
jaco node remove <hostname> [--force] [--server <host:port> --token <op>] [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (with `--server`)       |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                                |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `--force`             | `false`                       | skip drain enforcement                        |

### Auth

Operator token (TCP) or unix-socket trust (local).

### Behavior

Graceful by default: the scheduler reschedules every replica desired on
the leaving node onto eligible peers; once all replacements pass health
checks, the leaving node's containers stop and the node is removed
from raft membership. The leaving node MAY continue serving ingress
during the drain.

`--force` skips drain enforcement when the node hosts replicas pinned
to it that cannot be placed elsewhere. Without `--force`, such a remove
is rejected with `node hosts pinned replicas: [...]`.

### Exit codes

- `0` — node removed (drain may still be running on the cluster side
  for graceful removes).
- `1` — pinned-replica rejection, drain timeout, or auth/transport
  error.

### Examples

```sh
jaco node remove --server node-1:7000 node-3
# Removed node node-3
```

## `jaco node list`

### Synopsis

```
jaco node list --server <host:port> [--token <op>] [--ca-cert <path>]
```

### Flags

| flag                  | default                       | meaning                                  |
|-----------------------|-------------------------------|------------------------------------------|
| `--server <addr>`     | — (required)                  | leader or any node gRPC                  |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token                    |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                           |

### Auth

Operator token only (currently TCP-only; `--server` is required).

### Behavior

Prints one tab-separated line per member: `hostname  address  status`.
The table `status` is the raw enum (`NODE_STATUS_READY`).

`-o json` / `-o yaml` emit a `{"nodes": [...]}` object instead, with
`status` as a lowercase `snake_case` value (`ready`, `joining`,
`isolation_unavailable`, `drain_timeout`):

```json
{
  "nodes": [
    { "hostname": "node-1", "address": "10.0.0.5:7001", "status": "ready" }
  ]
}
```

### Exit codes

- `0` — list printed.
- `1` — auth or transport error.

### Examples

```sh
jaco node list --server node-1:7000
# node-1  10.0.0.5:7001  NODE_STATUS_READY
# node-2  10.0.0.6:7001  NODE_STATUS_READY
# node-3  10.0.0.7:7001  NODE_STATUS_READY
```

## See also

- [`jaco cluster`](cluster.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
- [Auth and tokens](../concepts/auth-and-tokens.md)
