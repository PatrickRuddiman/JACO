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
jaco node issue-join-token --node-name <hostname> [--san <DNS-or-IP> ...] [--server <host:port> --token <op> --ca-cert <path>] [--show-ca] [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--node-name <hostname>` | — (required)               | approved joining daemon identity; must match its OS hostname or explicitly configured hostname |
| `--san <DNS-or-IP>`   | hostname implicitly included  | repeatable approval for advertised hosts and additional dialable IPs / DNS aliases; no ports |
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (required with `--server`) |
| `--ca-cert <path>`    | `JACO_CA_CERT` or `/var/lib/jaco/node/ca.crt` | independently provisioned cluster CA PEM bundle (used with `--server`) |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `--show-ca`           | `false`                       | append the cluster CA PEM to the output       |

### Auth

Operator token over CA- and SAN-verifying TLS (TCP path), or unix-socket
trust (local path). Use an authenticated existing member to issue tokens
and obtain the public cluster CA.

### Behavior

Mints a single-use, 24-hour-TTL join token scoped to `--node-name` and
the approved SANs. The hashed secret and scope are stored in raft as a
`JoinToken{}` entity; the plaintext is printed once.

The hostname is implicitly approved. Supply `--san` for **every other
host component** advertised for Raft and gRPC, plus any additional
private/interface IP or DNS alias peers or operators will dial. Check
the joining daemon's hostname and configured addresses before issuance.
Signing uses only this approved set, not arbitrary aliases requested
in a CSR. Issue a separate token for each node. Unknown or legacy
unscoped tokens are rejected; reissue them with the required scope.

Output includes a `jaco node join` command with
`--ca-cert=/path/to/cluster-ca.crt`. Replace the placeholder with the
independently provisioned file. With `--show-ca`, the public cluster CA
PEM is appended. Obtain it through the existing member's local socket
or an already CA-verifying operator connection, then transfer it using
verified SSH or trusted configuration management **before** joining.
The certificate is public; its authenticity is essential. A join
response is not a source of initial trust.

### Exit codes

- `0` — token issued.
- `1` — missing/invalid node identity or SAN, auth failure, or transport error.

### Examples

```sh
export JACO_TOKEN=<operator_token>
jaco node issue-join-token --server node-1:7000 --ca-cert /path/to/cluster-ca.crt \
  --node-name node-2 --san 10.0.0.6 --san node-2.internal --show-ca
# Join token issued. On the joining node, run:
#
#   sudo jaco node join --peer=node-1:7000 --token=<single-use> --ca-cert=/path/to/cluster-ca.crt
#
# Token expires in 24h (single-use).
```

## `jaco node join`

### Synopsis

```
sudo jaco node join --peer <host:port> --token <single-use> --ca-cert <path> [--socket <path>] [--no-systemd-enable]
```

### Flags

| flag                  | default                       | meaning                                  |
|-----------------------|-------------------------------|------------------------------------------|
| `--peer <addr>`       | — (required)                  | leader or any cluster member's gRPC      |
| `--token <s>`         | `JACO_JOIN_TOKEN`             | single-use join token                    |
| `--ca-cert <path>`    | `JACO_CA_CERT` or `/var/lib/jaco/node/ca.crt` | required, independently provisioned CA PEM bundle; the flag may be omitted only when the env/default file is usable |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                  |
| `--no-systemd-enable` | `false`                       | skip `systemctl enable jaco` after join  |

### Auth

Unix-socket only. The CLI reads the CA file and sends its PEM bytes as
`ClusterJoinRequest.ca_cert` to `Cluster.Join` on the local daemon; the
daemon performs the cross-host enrollment. The token authorizes that
enrollment; it does **not** authenticate the remote server.

### Behavior

The local daemon generates a CSR and verifies `--peer` using the supplied
CA bundle, certificate validity, server-auth usage, and the exact dial
IP/DNS SAN **before sending the join token**. Missing/invalid CA files
fail explicitly; there is no fetch-and-trust, self-signed bootstrap
exception, or insecure fallback.

The leader checks token scope, expiry, consumption, and the CSR signature,
then returns a signed node cert, cluster CA, and raft peer set. Before
persisting credentials or starting Raft, the joiner verifies that the
returned CA belongs to the supplied bundle and that the leaf matches the
local private key, approved identity, advertised hosts, and both server-
and client-auth EKUs. It persists the **independently supplied bundle**,
not a server-provided replacement, under `$JACO_DATA_DIR/node/` with its
node credentials. The single-use token is marked `consumed_at` in raft
and cannot be reused.

`node join` is the operator's "this node is now a cluster member"
commitment, so by default it also runs `systemctl enable jaco` after a
successful join — otherwise a reboot would silently drop this node back out of
the cluster (the deb installs the unit disabled on purpose so half-configured
nodes never auto-start). The enable is best-effort: a friendly no-op where
`systemctl` is absent (e.g. Alpine/apk), and a warning-with-manual-fix rather
than a hard failure if enabling errors. Pass `--no-systemd-enable` to skip it.

### Exit codes

- `0` — node joined.
- `1` — missing/invalid CA, TLS or issued-certificate verification failure,
  bad/expired/consumed/unscoped token, network unreachable, or
  `cluster_already_initialized`.

### Examples

```sh
# First securely provision /path/to/cluster-ca.crt from an authenticated member.
sudo jaco node join --peer node-1:7000 --token <single-use> --ca-cert /path/to/cluster-ca.crt
# Joined cluster.
# Enabled jaco.service to start on boot — this node now survives reboot.
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
