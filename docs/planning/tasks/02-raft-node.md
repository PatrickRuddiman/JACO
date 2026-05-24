Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 00, 01

# Task 02 — raft-node

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Wire `hashicorp/raft` v1.7+ with `raft-boltdb-v2` log/stable store and the bundled TCP transport into `internal/controlplane/raft/`.

## Tasks
- [x] Add `github.com/hashicorp/raft` and `github.com/hashicorp/raft-boltdb/v2` to `go.mod`.
- [x] Create `internal/controlplane/raft/node.go` defining `type Node struct { Raft *hraft.Raft; logStore hraft.LogStore; stableStore hraft.StableStore; snapStore hraft.SnapshotStore; transport hraft.Transport }`. Package name `raftnode` (avoids collision with the `hraft` alias for hashicorp/raft).
- [x] In `internal/controlplane/raft/node.go`, expose `New(cfg Config) (*Node, error)` where `Config` carries `DataDir`, `BindAddr`, `LocalID`, `Bootstrap bool`, `FSM hraft.FSM`, optional `LogOutput`. (Defaulting `BindAddr` to `0.0.0.0:7000` is deferred to the daemon entry-point task; here `BindAddr` is mandatory.)
- [x] Use `boltdb.NewBoltStore(filepath.Join(DataDir, "raft", "log.db"))` for log + stable store. Use `hraft.NewFileSnapshotStore(filepath.Join(DataDir, "raft"), 3, logOut)`.
- [x] Use `hraft.NewTCPTransport(BindAddr, nil, 3, 10*time.Second, logOut)`. Configure `HeartbeatTimeout: 250ms, ElectionTimeout: 1s, CommitTimeout: 50ms, LeaderLeaseTimeout: 250ms, SnapshotInterval: 120s, SnapshotThreshold: 8192`.
- [x] Expose `Apply(cmd []byte, timeout time.Duration) (uint64, error)` wrapping `Raft.Apply`; `timeout==0` falls through to the 5s default.
- [x] Expose `Leader() hraft.ServerAddress` and `IsLeader() bool`; also `LocalAddr()` and `Shutdown()` (needed by tests and future graceful-stop wiring).
- [x] Create `internal/controlplane/raft/node_test.go` that boots a single-node cluster with `Bootstrap: true` against `t.TempDir()`, waits for leadership, and asserts `Apply([]byte("noop"), 1*time.Second)` returns `index > 0`. Plus a table test for required-field validation and a default-timeout regression test.

## Acceptance criteria
- [x] `go test ./internal/controlplane/raft/... -race -count=1` exits 0.
- [x] `go vet ./internal/controlplane/raft/...` exits 0.
- [x] `grep -q 'github.com/hashicorp/raft ' go.mod && grep -q 'github.com/hashicorp/raft-boltdb/v2' go.mod`.
- [x] `git grep -nE 'HeartbeatTimeout|ElectionTimeout' internal/controlplane/raft/node.go` matches both.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
