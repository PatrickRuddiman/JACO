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

The tarball includes the cluster CA **private key**, resolved Compose
configuration (which can contain secrets), registry credentials, ACME/TLS
private material, and SHA-256-hashed operator tokens. Treat it like a
credential store:

- Encrypt at rest before uploading anywhere.
- Restrict access to the same humans who hold operator tokens.
- Use a private backup directory (`0700` on Unix, or a private Windows ACL).
- Keep a recent local copy and a remote copy; lose neither.

On Unix, `jaco backup` creates its staging file with mode `0600` (or stricter
under the caller's umask), before any snapshot bytes are written. It syncs
and closes that file before atomically replacing a regular destination.
It never truncates or chmods an existing archive in place. Other hard links
to the old inode keep their old contents and permissions. Symlink and
special-file destinations are rejected rather than followed or opened.

Staging, publication, and cleanup are relative to an open directory handle,
so a directory rename cannot redirect them. The directory must be owned
by the invoking user or root. Non-sticky group/world-writable directories
are rejected because another user could replace the staging entry; trusted
sticky directories such as `/tmp` remain usable. Windows inherits directory
ACLs instead of enforcing Unix `0600`/ownership rules, and the Unix atomic
replacement guarantee does not apply there. Choose a private Windows
directory before exporting.

Failed or canceled exports preserve the prior destination and remove the
incomplete staging file; cleanup failures are reported. Abrupt process
termination or a host crash may leave an owner-only `.jaco-backup-*` staging
file. Inspect such files and remove only identified incomplete exports
after confirming no backup is running.

### Archives created by older releases

Upgrading protects newly written exports, not existing archives or their
copies. Inventory retained local and remote copies, verify file ownership
and links in an owner-controlled directory, and restrict existing regular
archives to `0600` and their directory to `0700` (or equivalent Windows
ACLs). Review unexpected symlinks or hard links before changing permissions;
do not apply a blanket recursive chmod or deletion. Replacing one archive
does not tighten permissions on a separate copy or on an old hard link.

Permissions, relocation, or encryption cannot retract data already read or
copied. If unauthorized access was possible, treat embedded secrets as
exposed: rotate affected registry/application credentials and reusable
tokens, and plan replacement of exposed CA, ACME, and TLS keys/certificates
with the corresponding cluster trust rollout or recovery procedure.
Protect every retained copy; creating a new restricted backup is not
credential rotation.

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
