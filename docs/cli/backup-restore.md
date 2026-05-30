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
```

### Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--input <file>`      | — (required)                  | backup tarball                                |
| `--name <s>`          | — (required)                  | hostname / raft local-id for this node        |

`JACO_DATA_DIR` overrides the target data directory (default
`/var/lib/jaco`).

### Auth

Filesystem only — `restore` writes to disk locally; it does not RPC
the daemon. Run as root on the receiving host with `jacod` **stopped**.

### Behavior

Primes the data directory from the backup: validates the metadata
against the daemon's version, seeds a fresh raft store from
`snapshot.bin`, and writes a marker so the daemon emits a
`RESTORE_COMPLETED` audit event on its first boot.

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
sudo jaco restore --input cluster-2026-05-25.tar.gz --name $(hostname)
sudo systemctl start jaco
jaco cluster status
```

## See also

- [Backups walkthrough](../operations/backups.md)
- [Recovery](../operations/recovery.md)
- [`jaco cluster init`](cluster.md)
