Parent slice: [control-plane](../../slices/issue-28/control-plane.md)
Depends on: 00, 01

# Task 02 — ensure-subnet-rpc

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Add a leader-gated `Internal.EnsureSubnet` RPC that allocates a per-host `/24`, returns its CIDR (or a `subnet_pool_exhausted` error), and logs pool utilization.

## Tasks
- [x] `proto/jaco/v1/services.proto:256` — add `rpc EnsureSubnet(EnsureSubnetRequest) returns (EnsureSubnetResponse);` to `service Internal`; define `EnsureSubnetRequest{ string deployment = 1; string network = 2; string host = 3; }` and `EnsureSubnetResponse{ string cidr = 1; }`. Run `make proto`.
- [x] `internal/daemon/grpc/server.go` (`Server` struct ~line 50) — add an `ipamPool string` field; set it from the daemon config in the `cmd/jacod` Server constructor (the config pool isn't plumbed into `Server` today). Default to `ipam.DefaultPoolCIDR` when empty.
- [x] `internal/daemon/grpc/internal.go:31` — add an `EnsureSubnet` handler mirroring `Submit`'s leader gate (return `codes.Unavailable` "no_leader" when `!r.IsLeader()`). Build `ipam.New(s.server.state, <raft.Apply applier>, s.server.ipamPool)` and call `Allocate(req.Deployment, req.Network, req.Host)`; return the CIDR.
- [x] `internal/daemon/grpc/internal.go` — when a new subnet was written, log pool utilization `len(state.Subnets) / 256`: WARN ≥75%, ERROR ≥90%, naming the `(deployment, network, host)` tuple. No log on an idempotent hit.
- [x] `internal/daemon/grpc/internal.go` — map `ipam.IsExhausted(err)` to a gRPC error whose status carries code `subnet_pool_exhausted`.

## Acceptance criteria
- [x] `make proto` exits 0; `go build ./...` exits 0.
- [x] `go test ./internal/daemon/grpc/ -race -count=1` passes (integration: EnsureSubnet returns a CIDR on the leader; returns `Unavailable`/`no_leader` when not leader; returns `subnet_pool_exhausted` when the pool is full).
- [x] `git grep -n 'rpc EnsureSubnet' proto/jaco/v1/services.proto` matches 1.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
