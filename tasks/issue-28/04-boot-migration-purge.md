Parent slice: [control-plane](../../slices/issue-28/control-plane.md)
Depends on: 01

# Task 04 — boot-migration-purge

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
On daemon upgrade, the leader purges pre-fix host-less subnets once so reconcilers re-allocate per-host `/24`s.

## Tasks
- [ ] Add `internal/daemon/grpc/subnet_migration.go` with a pure helper `purgeHostlessSubnets(st *state.State, apply func([]byte) error) (purged int, err error)` that scans `st.Subnets.List()`, and for each entry with empty `GetHost()` raft-applies a `SubnetFree` for its `(deployment, network, "")`-style key. Returns the count.
- [ ] `internal/daemon/grpc/server.go:363` (`startSubsystems`) — spawn a goroutine that polls `node.IsLeader()` until this node is leader (and the FSM has applied past its last index), then calls `purgeHostlessSubnets` exactly once, guarded by a `sync.Once`-style flag so re-election doesn't repeat it. Log the purged count.

## Acceptance criteria
- [ ] `go test ./internal/daemon/grpc/ -race -count=1` passes (unit: `purgeHostlessSubnets` frees only entries with empty host, leaves host-bearing entries, and returns the right count using a recording apply fake).
- [ ] `go build ./...` exits 0.
- [ ] `test -f internal/daemon/grpc/subnet_migration.go`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
