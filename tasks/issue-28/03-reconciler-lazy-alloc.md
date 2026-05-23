Parent slice: [control-plane](../../slices/issue-28/control-plane.md)
Depends on: 02

# Task 03 — reconciler-lazy-alloc

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Trigger per-host subnet allocation lazily in the reconciler via a leader-aware `ensureSubnet` function, distinguishing a transient leader blip from real pool exhaustion.

## Tasks
- [ ] `internal/runtime/reconciler/reconciler.go:67` — add an injected field on `Reconciler` and a `New` parameter `ensureSubnet func(ctx context.Context, deployment, network, host string) (cidr string, err error)`.
- [ ] `internal/daemon/grpc/server.go:599` (reconciler construction) — build the `ensureSubnet` closure mirroring the existing `submit` closure's leader/forward shape (`server.go:556`): when `node.IsLeader()`, call `ipam.Allocate` directly; otherwise dial the leader via `leaderGRPCAddr(st, node)` (`server.go:798`) and call `Internal.EnsureSubnet`. Surface the gRPC `no_leader` status and the `subnet_pool_exhausted` status as distinct, classifiable errors.
- [ ] `internal/runtime/reconciler/reconciler.go:252` — in `startReplica`, before `bridge.Ensure`, call `ensureSubnet(ctx, rep.GetDeployment(), netSuffix, r.hostname)` and pass the returned CIDR into `bridge.Ensure` (replacing the `state.Subnets.Get` CIDR read at line 253–257).
- [ ] `internal/runtime/reconciler/reconciler.go` — error handling in `startReplica`: on `subnet_pool_exhausted`, emit `ReplicaObserved{Id, State: REPLICA_STATE_FAILED, Code: "subnet_pool_exhausted", Details: {deployment, network, host}}` via the existing submit fn and return without starting the container; on `no_leader`/`Unavailable`, log and return without emitting FAILED (retried by the watch / 30s safety tick).

## Acceptance criteria
- [ ] `go test ./internal/runtime/reconciler/ -race -count=1` passes (unit with a fake `ensureSubnet`: success path calls `bridge.Ensure` with the returned CIDR; `subnet_pool_exhausted` emits a FAILED ReplicaObserved and starts no container; `no_leader` emits no observation and starts no container).
- [ ] `go build ./...` exits 0.
- [ ] `git grep -n 'ensureSubnet' internal/runtime/reconciler/reconciler.go internal/daemon/grpc/server.go` matches ≥ 2.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
