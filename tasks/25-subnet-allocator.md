Parent slice: [discovery](../slices/discovery.md)
Depends on: 04, 14

# Task 25 — subnet-allocator

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Leader-only IPAM that hands out a `/24` per `(deployment, network)` from the cluster `/16` pool; free on deployment delete; pool exhaustion surfaces a typed error.

## Tasks
- [ ] Create `internal/discovery/ipam/ipam.go` with `EnsureSubnets(ctx, store *state.Store, raftApply func([]byte) error, deployment string, networks []string) error`. For each (deployment, network) missing from `store.Subnets`, raft-Apply `Command{SubnetAllocate}{deployment, network, cidr}`. Leader-forwarded internally (the call lives inside Deploy.Apply, which already runs on leader after forwarding).
- [ ] Allocator: cluster pool default `10.42.0.0/16`, configurable via daemon flag `--cluster-cidr`. Maintain a free-bitmap of 256 /24 slots in `state.Subnets` (derived on read; no separate persistent bitmap). Pick the lowest-numbered free slot.
- [ ] `_default` network counts as one allocation per deployment.
- [ ] Free path: on `Command{DeploymentDelete}`, FSM cascades by raft-Applying `Command{SubnetFree}` for each owning subnet (or the FSM does both in one Apply — implementation choice, but observable via watch).
- [ ] Pool exhaustion: `EnsureSubnets` returns `Error{code: "subnet_pool_exhausted", details: {pool, allocated_count}}`.
- [ ] Hook into Deploy.Apply (task 14) so subnets are visible to discovery watchers immediately on apply (before ReplicaDesired writes land).
- [ ] Unit tests: allocate 256 subnets succeeds, 257th errors with `subnet_pool_exhausted`; free a middle subnet, next allocation reuses it (lowest-free).

## Acceptance criteria
- [ ] `go test ./internal/discovery/ipam/... -race -count=1` exits 0.
- [ ] Test asserts `subnet_pool_exhausted` error after 256 allocations on the default pool.
- [ ] Test asserts free + re-allocate reuses the lowest-numbered free /24.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
