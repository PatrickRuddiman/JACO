---
sources:
  - cmd/jaco/backup.go
  - cmd/jaco/restore.go
  - internal/controlplane/backup/
  - internal/controlplane/raft/
---

# Backups

Cluster state is held in the raft FSM: deployments, replicas, routes,
certs, tokens, the audit log, IPAM allocations, scheduler bookkeeping.
`jaco backup` writes a consistent snapshot of all of that to a single
tarball. `jaco restore` primes a fresh node from one.

CLI: [`jaco backup`](../cli/backup-restore.md),
[`jaco restore`](../cli/backup-restore.md).

## What's in the tarball

- `snapshot.bin` — the raw raft snapshot bytes from
  `raft.Snapshot()`.
- `meta.json` — `{cluster_id, snapshot_index, snapshot_term,
  jaco_version, taken_at, leader_at_snapshot}`.

The snapshot is consistent at a **single raft commit index**: every
deployment, audit event, and cert that committed before that index is
present; nothing committed after is. The cluster CA cert and key are
included — restoring on a fresh host stands up the same cluster
identity.

Container state on the original nodes is **not** in the tarball. After
restore, the runtime on the restored cluster re-pulls images and
re-creates containers per the desired state. Plan for the pull window.

## Take a backup

```sh
export JACO_TOKEN=<operator_token>
export LEADER=node-1:7000

jaco backup --server $LEADER --output cluster-$(date +%F).tar.gz
# Wrote 41279 bytes to cluster-2026-05-25.tar.gz
```

A schedule running on any operator host (cron, systemd timer) is the
expected pattern. The RPC streams chunks; the CLI has a 5-minute
deadline.

A `backup_taken` audit event is recorded with the resulting snapshot
index.

## Store backups safely

The tarball includes the cluster CA **private key** and the
SHA-256-hashed operator tokens. Treat it like a credential store:

- Encrypt at rest before uploading anywhere.
- Restrict access to the same humans who hold operator tokens.
- Keep a recent local copy and a remote copy; lose neither.

Plaintext operator tokens are NOT in the backup (only their hashes).
After restoring on a fresh cluster, the original tokens still
authenticate — they hash to the same values.

## Restore on a fresh host

The receiving host MUST have:

- `jaco` + `jacod` installed at a **compatible version** with the
  taken-at version (same major).
- An empty `$JACO_DATA_DIR` (default `/var/lib/jaco`). Restore refuses
  to overwrite an existing data dir.
- The daemon **stopped**: `sudo systemctl stop jaco`.

Then:

```sh
sudo systemctl stop jaco
sudo jaco restore --input cluster-2026-05-25.tar.gz --name $(hostname)
sudo systemctl start jaco
jaco cluster status
```

The receiving node bootstraps the raft store from the snapshot, starts
as a single voter with the same cluster id, and emits a
`RESTORE_COMPLETED` audit event on first boot.

## Rejoin the rest of the cluster

Other nodes rejoin via the usual flow:

```sh
# on the restored node
JACO_TOKEN=<operator_token> jaco node issue-join-token
```

then on each other node:

```sh
sudo jaco node join --peer <restored-node>:7000 --token <single-use>
```

Once every node is back as `READY`, the cluster is fully restored.
Deployments, routes, certs, and IPAM allocations come back exactly as
they were at the snapshot's raft index.

## Drill it

The first time you need a restore should not be in production. Drill
the round-trip at least once before going live:

1. Take a backup of a working cluster.
2. On a separate host (fresh VM, container), run the restore +
   `systemctl start` flow.
3. Confirm `jaco status` shows the deployments and `jaco audit` shows
   the historical events.
4. Wipe the drill host.

## See also

- [`jaco backup` / `jaco restore`](../cli/backup-restore.md)
- [Recovery](recovery.md)
- [Auth and tokens](../concepts/auth-and-tokens.md)
