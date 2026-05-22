Parent slice: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md)
Depends on: 04, 06

# Task 10 — backup-restore

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`jaco backup` / `jaco restore` workflow producing a raft-snapshot tarball with metadata and re-seeding a fresh raft store from it.

## Tasks
- [x] Create `internal/controlplane/backup/backup.go` exposing `Export(opts ExportOptions) error`: trigger `Raft.Snapshot()`, read the snapshot bytes via `SnapshotFuture.Open()`, write `meta.json` + `snapshot.bin` into a gzipped tarball. Meta shape: `{schema_version, cluster_id, snapshot_index, snapshot_term, jaco_version, taken_at, leader_at_snapshot}`. Best-effort raft-Apply of an `AuditAppend{BACKUP_TAKEN}` after the tarball is written.
- [x] In the same package, `Import(opts ImportOptions) error`: untar, validate `schema_version` and major-version compatibility, refuse to overwrite an existing log store, place the snapshot in `hraft.NewFileSnapshotStore` via `sink.Write` + `Close`, then call `hraft.RecoverCluster` to set the cluster configuration to just the restoring node. Writes a `restore.txt` marker the daemon (task 17) reads on first boot to emit `RESTORE_COMPLETED`.
- [x] Add `Cluster.Backup` stream handler in `internal/controlplane/grpc/backup.go`. Streams the tarball back in 64 KiB chunks. Requires raft leader.
- [ ] `Cluster.Restore` is intentionally Unimplemented at the gRPC layer in v1 — the operator runs `jaco restore` locally on the receiving node before `jaco serve` boots; there's no in-flight cross-cluster restore path in v1. Documented in `internal/controlplane/grpc/backup.go`.
- [x] Create `cmd/jaco/backup.go` (`jaco backup --output cluster.tar.gz`) which dials `Cluster.Backup`, accumulates chunks, writes the file.
- [x] Create `cmd/jaco/restore.go` (`jaco restore --input cluster.tar.gz --name <hostname>`) which operates on the local data dir via `backup.Import` — no cluster dial.
- [x] Create `internal/controlplane/backup/backup_test.go` covering: round-trip Export → Import → reopen raft → assert `Token{identity:"bootstrap"}` and `ClusterMeta.cluster_id` survive; tarball entries are exactly `{meta.json, snapshot.bin}`; refuse-overwrite-existing-state; reject schema_version mismatch; reject backups missing entries.
- [x] BACKUP_TAKEN audit emission is wired in Export (best-effort). RESTORE_COMPLETED emission deferred to task 17 via the `restore.txt` marker the daemon consumes on first boot.

## Acceptance criteria
- [x] `go test ./internal/controlplane/backup/... -race -count=1` exits 0.
- [x] Test asserts restored cluster's `state.Tokens.Get("bootstrap")` returns ok and `ClusterMeta.cluster_id` matches.
- [x] Test asserts the tarball entries equal `{meta.json, snapshot.bin}` (equivalent to the shell `tar -tzf <output> | sort`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
