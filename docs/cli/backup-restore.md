---
sources:
  - cmd/jaco/backup.go
  - cmd/jaco/restore.go
  - internal/controlplane/backup/
---

# `jaco backup` and `jaco restore`

Cluster state export and restore. The backup tarball is a raft snapshot
plus metadata; restore primes a fresh data directory so a single node
can be the seed of a recovered cluster.

## `jaco backup`

### Synopsis

```
jaco backup --output <file>
            [--server <host:port> --token <op>]
            [--ca-cert <path>] [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (with `--server`)       |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                                |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `--output <file>`     | — (required)                  | destination tarball (e.g. `cluster.tar.gz`)   |

### Auth

Operator token (TCP) or unix-socket trust (local).

### Behavior

Streams a fresh raft snapshot plus a `meta.json` (cluster id, snapshot
index/term, JACO version, taken-at timestamp, leader-at-snapshot) into
a gzipped tar file at `--output`. The RPC has a 5-minute deadline; the
CLI prints the total byte count on success.

The snapshot is consistent at a single raft commit index, so the
restored cluster will reflect every deployment committed before that
index and none committed after.

### Content encryption

Schema-2 archives contain an authenticated encrypted `snapshot.bin`.
Its envelope also authenticates `meta.json`; wrapping keys are never
included. Export does not require handing the daemon's keyring to the
CLI. Restore does require independent access to the matching external keys.
See [state encryption and legacy conversion](../operations/state-encryption.md).

### Exit codes

- `0` — backup written.
- `1` — auth, transport, or write error.

### Examples

```sh
jaco backup --server $LEADER --output cluster-$(date +%F).tar.gz
# Wrote 41279 bytes to cluster-2026-05-25.tar.gz
```

## `jaco restore`

### Synopsis

```
sudo jaco restore --input <file> --name <hostname>
                  [--key-file <external-keyring>] [--data-dir <fresh-directory>]
```

### Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--input <file>`      | — (required)                  | backup tarball                                |
| `--name <s>`          | — (required)                  | hostname / raft local-id for this node        |
| `--key-file <path>`   | environment/service credential | independently provisioned external state keyring |
| `--data-dir <path>`   | `JACO_DATA_DIR` or `/var/lib/jaco` | fresh destination; existing artifacts are not overwritten |

`JACO_DATA_DIR` overrides the target data directory (default
`/var/lib/jaco`).

### Auth

Filesystem only — `restore` writes to disk locally; it does not RPC
the daemon. Run as root on the receiving host with `jacod` **stopped**.

### Behavior

Authenticates metadata and snapshot before writing state, checks the
version, and seeds a fresh encrypted Raft store. It retains local
restoration metadata in `restore.txt`; this is not an automatic audit-event
emission.

Missing/wrong keys and tampering fail closed. Schema-1 plaintext archives
must first be explicitly converted with `jaco state reencrypt-backup
--allow-legacy-plaintext`; restore never silently accepts them. Failed
filesystem copies retain an incomplete-state guard and cannot be started.
The daemon must receive the same complete keyring before restart.

After restore, start the daemon and confirm the cluster comes up as a
single voter:

```sh
sudo systemctl start jaco
jaco cluster status
```

Additional nodes rejoin via the usual `jaco node join` flow.

### Exit codes

- `0` — restored; next step is to start the daemon.
- `1` — bad input, version mismatch, or filesystem error.

### Examples

```sh
sudo systemctl stop jaco
sudo jaco restore --input cluster-2026-05-25.tar.gz --name $(hostname) \
  --key-file /etc/jaco/keys/cluster-v1.json
sudo systemctl start jaco
jaco cluster status
```

## See also

- [Backups walkthrough](../operations/backups.md)
- [Recovery](../operations/recovery.md)
- [State encryption, key custody and migration](../operations/state-encryption.md)
- [`jaco cluster init`](cluster.md)
