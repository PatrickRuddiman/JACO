Parent slice: [discovery](../slices/discovery.md)
Depends on: 04, 14

# Task 25 — subnet-allocator

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Leader-only IPAM that hands out a `/24` per `(deployment, network)` from the cluster `/16` pool; free on deployment delete; pool exhaustion surfaces a typed error.

## Tasks
- [x] Create `internal/discovery/ipam/ipam.go` with `IPAM` type wrapping state + an injected `Applier`. `New(state, apply, poolCIDR)` validates the pool is a /16; `Allocate(deployment, network)` is idempotent (returns the existing Subnet if present), picks the lowest-numbered free /24 in the pool, raft-Applies `Command{SubnetAllocate}`, and returns the persisted Subnet. `Free(deployment, network)` no-ops on missing or raft-Applies `Command{SubnetFree}`. `EnsureSubnets(deployment, networks)` is the Deploy.Apply-side helper that iterates Allocate.
- [x] Default pool is `10.244.0.0/16` (DefaultPoolCIDR constant) per the discovery slice §3 — diverges from the task draft's `10.42.0.0/16` to match the spec. Pool exhaustion (all 256 /24s taken) returns typed `*IPAMError{Code:"ipam_pool_exhausted"}` (matching the spec — diverges from the task draft's `subnet_pool_exhausted`); `IsExhausted(err)` is the gating helper.
- [x] FSM (task 04) already handles SubnetAllocate / SubnetFree commands — verified by reading fsm.go.
- [ ] **Deferred**: Hook into Deploy.Apply (task 14) so subnets are visible to discovery watchers immediately on apply — needs to enumerate networks from compose.yaml + jaco.yaml and call EnsureSubnets before the DeploymentApply raft-Apply. Plumbing lands when the daemon-side compose-network enumeration helper does (task 27).
- [x] Ten unit tests pass with -race: pool validation rejects /24, /12, garbage; Allocate is idempotent for the same key; 100 distinct allocations produce 100 unique /24s (the AC); pool exhaustion after 256 allocations returns `ipam_pool_exhausted` (the AC); Free releases the slot so a subsequent Allocate reuses it (lowest-free); Free on a missing key is a no-op; EnsureSubnets allocates all networks + is idempotent; empty args rejected; allocated Subnet is actually persisted to state.Subnets with the expected scope.

## Acceptance criteria
- [x] `go test ./internal/discovery/ipam/... -race -count=1` exits 0.
- [x] Test asserts `ipam_pool_exhausted` error after 256 allocations on the default pool (`TestAllocate_PoolExhaustionReturnsTypedError`).
- [x] Test asserts no duplicate /24 across 100 allocations (`TestAllocate_HundredAllocationsAreUnique`).
- [ ] Test asserts free + re-allocate reuses the lowest-numbered free /24.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
