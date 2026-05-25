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
leader (if any), the latest raft index, and every member node with its
status (`READY`, `JOINING`, `ISOLATION_UNAVAILABLE`, etc.).

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
#   - node-1 @ 10.0.0.5:7001 [READY]
#   - node-2 @ 10.0.0.6:7001 [READY]
#   - node-3 @ 10.0.0.7:7001 [READY]
```

## See also

- [`jaco node`](node.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
- [Getting started](../getting-started.md)
