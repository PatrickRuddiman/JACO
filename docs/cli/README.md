---
sources:
  - cmd/jaco/
  - internal/cliclient/
---

# CLI reference

`jaco` is the operator and developer CLI. It drives a daemon either via
the **local unix socket** (`/var/run/jaco/jaco.sock`) or via a peer
node's **cross-host gRPC** listener (default `:7000`). The daemon side
of every RPC is implemented by `jacod`; see
[Architecture](../concepts/architecture.md).

## Two ways to dial

Every operator subcommand that mutates state honors a uniform dial
shape: it tries the local unix socket unless you pass `--server`.

- **Local socket** — on a cluster node, omit `--server`. The CLI dials
  `--socket` (default `/var/run/jaco/jaco.sock`, override via
  `JACO_SOCKET`). The daemon trusts the peer by filesystem
  permissions, so **no bearer token is required**; the action is
  attributed to the `local` identity in the audit log.
- **Remote** — off-node, pass `--server <host:port>` plus `--token`
  (or `JACO_TOKEN`). The CLI dials TLS gRPC and pins the cluster CA
  from `--ca-cert` (default `/var/lib/jaco/node/ca.crt`, override
  via `JACO_CA_CERT`). Without a `--ca-cert`, the dial falls back to
  `InsecureSkipVerify` with a one-line stderr warning — fine for v0
  bootstrapping, but pin the cert in any environment you care about.

A handful of commands (`rollback`, `delete`, `token *`, `node list`)
currently require `--server` even when run on a cluster node. The
remainder (`apply`, `status`, `logs`, `audit`, `backup`,
`node remove`, `node issue-join-token`) accept either transport.

## Subcommands

| command                         | purpose                                    |
|---------------------------------|--------------------------------------------|
| [`jaco cluster init`](cluster.md)            | bootstrap a new cluster on this node       |
| [`jaco cluster status`](cluster.md)          | print the local daemon's cluster status    |
| [`jaco node join`](node.md)                  | attach this node to an existing cluster    |
| [`jaco node issue-join-token`](node.md)      | mint a single-use 24h join token           |
| [`jaco node list`](node.md)                  | list cluster members                       |
| [`jaco node remove`](node.md)                | remove a node from the cluster             |
| [`jaco apply`](apply.md)                     | apply a jaco.yaml + compose pair           |
| [`jaco status`](status.md)                   | snapshot deployments, replicas, routes     |
| [`jaco logs`](logs.md)                       | stream container logs across replicas      |
| [`jaco rollback`](rollback-delete.md)        | roll a deployment back one revision        |
| [`jaco delete`](rollback-delete.md)          | remove a deployment and its routes/certs   |
| [`jaco token issue`](token.md)               | mint a new operator token                  |
| [`jaco token revoke`](token.md)              | revoke an operator token by identity       |
| [`jaco token list`](token.md)                | list known operator tokens                 |
| [`jaco audit`](audit.md)                     | query the cluster audit log                |
| [`jaco backup`](backup-restore.md)           | stream a cluster backup tarball locally    |
| [`jaco restore`](backup-restore.md)          | restore a backup into this node's data dir |
| [`jaco self-upgrade`](self-upgrade.md)       | verify + atomically swap both binaries     |
| [`jaco validate`](validate.md)               | lint jaco.yaml / compose.yml locally       |
| `jaco version` / `jaco --version`            | print the CLI version (and `--version` flag) |

## Global flags

Registered on the root command in `cmd/jaco/root.go`:

| flag                  | env                | default | meaning                                                        |
|-----------------------|--------------------|---------|----------------------------------------------------------------|
| `--context <name>`    | —                  | —       | named cluster context (clusters-config support is in progress) |
| `-o, --output <fmt>`  | —                  | `table` | output format. Currently honored only by `jaco audit` (`table` / `json`); every other subcommand rejects non-default values with `output format "<fmt>" not implemented yet; only "table" is supported (#156)` to fail loudly on CI scripts that parse the wrong shape |
| `--server <addr>`     | —                  | —       | single-shot server override; bypasses context                  |
| `-q, --quiet`         | —                  | `false` | suppress non-essential output                                  |
| `-v, --verbose`       | —                  | `false` | debug-level logs to stderr                                     |
| `--log-level <lvl>`   | `JACO_LOG`         | `warn`  | one of `debug | info | warn | error`                           |

Level precedence: `--log-level` > `--verbose` > `JACO_LOG` > `warn`.

## Environment variables

| env             | consumed by                                  | purpose                                              |
|-----------------|----------------------------------------------|------------------------------------------------------|
| `JACO_TOKEN`    | every command that takes `--token`           | operator bearer token (required with `--server`)     |
| `JACO_SOCKET`   | every command that takes `--socket`          | local daemon unix socket path                        |
| `JACO_CA_CERT`  | every command that takes `--ca-cert`         | path to the cluster CA cert PEM                      |
| `JACO_JOIN_TOKEN` | `jaco node join`                           | single-use join token                                |
| `JACO_DATA_DIR` | `jaco restore`                               | daemon data dir to seed                              |
| `JACO_LOG`      | root command                                 | base log level                                       |
| `JACO_CONFIG`   | `jacod` (daemon)                             | path to `jacod.yaml` (override `/etc/jaco/jacod.yaml`) |

## Auth model

- The **unix socket** is the trust boundary on-node. Mode `0660`, group
  `jaco`; anyone in the group can drive the daemon. Actions are
  attributed to the `local` identity in the audit log.
- **Operator tokens** are 64-character hex strings. The first one is
  printed once by `jaco cluster init`; subsequent ones are minted via
  `jaco token issue --name <identity>` and optionally carry an
  `allows_privileged` flag (`--allow-privileged`) that admits
  manifests using compose `privileged:` or `security_opt:`. Token
  revocation is a raft write, effective cluster-wide within one apply
  (well under 5 s).
- **Join tokens** are single-use, 24-hour TTL, hashed in raft state.
  Mint with `jaco node issue-join-token`, consume with
  `jaco node join`.

See [Auth and tokens](../concepts/auth-and-tokens.md) for the full
trust model.

## Exit codes

- `0` — success.
- `1` — any error. Typed errors from the daemon render as
  `Error: <code>: <message>` on stderr; transport errors render as the
  underlying gRPC failure. See
  [Status and errors](../concepts/status-and-errors.md) for the closed
  set of `code` values.

## See also

- [Getting started](../getting-started.md)
- [Status and errors](../concepts/status-and-errors.md)
- [Auth and tokens](../concepts/auth-and-tokens.md)
- [Manifest schema](../manifests/jaco-yaml.md)
