Parent slice: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md)
Depends on: 04, 06

# Task 10 — backup-restore

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`jaco backup` / `jaco restore` workflow producing a raft-snapshot tarball with metadata and re-seeding a fresh raft store from it.

## Tasks
- [ ] Create `internal/controlplane/backup/export.go` exposing `Export(node *raft.Node, w io.Writer) error`: trigger `node.Raft.Snapshot().Persist(...)`, write `snapshot.bin` and `meta.json` into a `tar.gz(w)`. Meta shape: `{cluster_id, snapshot_index, snapshot_term, jaco_version, taken_at, leader_at_snapshot}`.
- [ ] Create `internal/controlplane/backup/import.go` exposing `Import(dataDir string, r io.Reader) error`: untar; validate meta (jaco_version major must match running binary); seed a fresh bolt + snapshot store under `dataDir/raft/`; record metadata so subsequent `raft.New(..., Bootstrap: true)` boots cleanly.
- [ ] Add `Cluster.Backup(req) returns stream BackupChunk` and `Cluster.Restore(req stream BackupChunk) returns (RestoreResponse{cluster_id, snapshot_index})` handlers. `Restore` requires the daemon to be in a "restore mode" (no FSM running); enforce by exposing it only when started with `--restore`.
- [ ] Create `cmd/jaco/backup.go` (`jaco backup --output cluster.tar.gz`) and `cmd/jaco/restore.go` (`jaco restore --input cluster.tar.gz --name <hostname>`). Restore takes the place of `bootstrap` for the receiving node.
- [ ] Create `internal/controlplane/backup/backup_test.go`: bootstrap cluster A; apply a no-op operator-token issuance; Export to a buffer; Import into a fresh data dir; restart raft; assert `Token{identity:"bootstrap"}` is present in restored state.
- [ ] Emit audit events `BACKUP_TAKEN` (during Export) and `RESTORE_COMPLETED` (during Import, written after first FSM apply).

## Acceptance criteria
- [ ] `go test ./internal/controlplane/backup/... -race -count=1` exits 0.
- [ ] Test asserts restored cluster's `state.Tokens.List()` length matches the original.
- [ ] `tar -tzf <output> | sort` lists exactly `meta.json` and `snapshot.bin`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
