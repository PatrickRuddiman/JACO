Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 00, 01

# Task 02 — raft-node

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Wire `hashicorp/raft` v1.7+ with `raft-boltdb-v2` log/stable store and the bundled TCP transport into `internal/controlplane/raft/`.

## Tasks
- [ ] Add `github.com/hashicorp/raft` and `github.com/hashicorp/raft-boltdb/v2` to `go.mod`.
- [ ] Create `internal/controlplane/raft/node.go` defining `type Node struct { Raft *raft.Raft; logStore raft.LogStore; stableStore raft.StableStore; snapStore raft.SnapshotStore; transport raft.Transport }`.
- [ ] In `internal/controlplane/raft/node.go`, expose `New(cfg Config) (*Node, error)` where `Config` carries `DataDir`, `BindAddr` (default `0.0.0.0:7000`), `LocalID`, `Bootstrap bool`, `FSM raft.FSM`.
- [ ] Use `boltdb.NewBoltStore(filepath.Join(DataDir, "raft", "log.db"))` for log + stable store. Use `raft.NewFileSnapshotStore(filepath.Join(DataDir, "raft", "snapshots"), 3, os.Stderr)`.
- [ ] Use `raft.NewTCPTransport(BindAddr, nil, 3, 10*time.Second, os.Stderr)`. Configure `raft.Config{HeartbeatTimeout: 250ms, ElectionTimeout: 1s, CommitTimeout: 50ms, LeaderLeaseTimeout: 250ms, SnapshotInterval: 120s, SnapshotThreshold: 8192}` (spec §4: 10s leader election bound).
- [ ] Expose `Apply(cmd []byte, timeout time.Duration) (uint64, error)` wrapping `raft.Apply`; default timeout 5s (spec §4 apply-to-steady-state 15s budget).
- [ ] Expose `Leader() raft.ServerAddress` and `IsLeader() bool`.
- [ ] Create `internal/controlplane/raft/node_test.go` that boots a single-node cluster with `Bootstrap: true` against `t.TempDir()`, waits for leadership, and asserts `Apply([]byte("noop"), 1*time.Second)` returns `index > 0`.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/raft/... -race -count=1` exits 0.
- [ ] `go vet ./internal/controlplane/raft/...` exits 0.
- [ ] `grep -q 'github.com/hashicorp/raft ' go.mod && grep -q 'github.com/hashicorp/raft-boltdb/v2' go.mod`.
- [ ] `git grep -nE 'HeartbeatTimeout|ElectionTimeout' internal/controlplane/raft/node.go` matches both.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
