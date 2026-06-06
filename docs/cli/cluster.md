---
sources:
  - cmd/jaco/cluster.go
  - internal/controlplane/grpc/cluster.go
  - internal/controlplane/bootstrap/
---

# `jaco cluster`

Local-daemon cluster control: initialize a new cluster, inspect this
node's cluster status. Both subcommands RPC the local `jacod` over its
unix socket — they are intended to be run on the host the daemon is
running on, as root.

## `jaco cluster init`

### Synopsis

```
sudo jaco cluster init [--name <cluster-name>] [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                |
|-----------------------|-------------------------------|----------------------------------------|
| `--name <s>`          | UUID                          | optional human-readable cluster name   |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                |

`JACO_SOCKET` overrides the default socket path.

### Auth

Unix-socket only. No bearer token; the socket's filesystem permissions
(mode `0660`, group `jaco`) are the trust boundary.

### Behavior

The daemon generates a cluster id and Ed25519 cluster CA, bootstraps
raft as a single-voter cluster, mints the first operator token under
identity `bootstrap`, and transitions out of the
`cluster_uninitialized` state. The operator token is printed once and
never retrievable from raft.

After this command returns, every other RPC works (token-gated on the
cross-host listener; socket-trust on the local listener).

### Exit codes

- `0` — cluster initialized.
- `1` — `cluster_already_initialized` if raft state is already present,
  or any transport error.

### Examples

```sh
sudo jaco cluster init
# Cluster initialized.
#   cluster_id:     7d4f...
#   operator_token: 2b1c...   (64 hex chars)
#
# Save the operator token now — it cannot be recovered.
```

Store the token immediately. It is the only operator credential that
exists until you mint more via [`jaco token issue`](token.md).

## `jaco cluster status`

### Synopsis

```
jaco cluster status [--socket <path>]
```

### Flags

| flag              | default                       | meaning                  |
|-------------------|-------------------------------|--------------------------|
| `--socket <path>` | `/var/run/jaco/jaco.sock`     | local jacod unix socket  |

### Auth

Unix-socket only. `Cluster.Status` is also allowed pre-init for
liveness probing; no token required.

### Behavior

Reports whether the local daemon is initialized, the current raft
leader (if any), the latest raft index, and every member node with
its status (`READY`, `JOINING`, `ISOLATION_UNAVAILABLE`, etc.) and its
raft **suffrage** (`VOTER` or `NONVOTER`).

The suffrage column is populated from `raft.GetConfiguration()` on the
leader at request time. On a follower the suffrage is rendered as `?`
because the follower's view of the raft configuration would be stale
across an election — the CLI refuses to mislead operators with a
suffrage value it can't vouch for. To see suffrages, run `jaco
cluster status` on the leader (the `Leader:` line tells you which
node it is).

On an uninitialized node, the output is:

```
Status:    uninitialized

Run `jaco cluster init` to start a new cluster,
or `jaco node join` to join an existing one.
```

### Exit codes

- `0` — status printed (initialized or not).
- `1` — transport error.

### Examples

```sh
jaco cluster status
# Status:     initialized
# Leader:     node-1
# Raft index: 4178
# Nodes (3):
#   - node-1 @ 10.0.0.5:7001 [READY, VOTER]
#   - node-2 @ 10.0.0.6:7001 [READY, VOTER]
#   - node-3 @ 10.0.0.7:7001 [READY, VOTER]
```

A 4-node cluster — the 4th node stays NONVOTER because
[the voter-set policy](../concepts/cluster-lifecycle.md#voter-set-policy)
caps voters at the next odd number below the member count:

```sh
jaco cluster status
# Status:     initialized
# Leader:     node-1
# Raft index: 6210
# Nodes (4):
#   - node-1 @ 10.0.0.5:7001 [READY, VOTER]
#   - node-2 @ 10.0.0.6:7001 [READY, VOTER]
#   - node-3 @ 10.0.0.7:7001 [READY, VOTER]
#   - node-4 @ 10.0.0.8:7001 [READY, NONVOTER]
```

## See also

- [`jaco node`](node.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
- [Getting started](../getting-started.md)
