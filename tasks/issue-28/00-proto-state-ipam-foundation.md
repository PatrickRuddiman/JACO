Parent slice: [control-plane](../../slices/issue-28/control-plane.md)
Depends on: none

# Task 00 — proto-state-ipam-foundation

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Add the `host` dimension to the subnet proto/state types and rework IPAM into a mutex-guarded per-host allocator, removing the now-incompatible apply-time allocation.

## Tasks
- [ ] `proto/jaco/v1/entities.proto:203` — add `string host = 5;` to message `Subnet`.
- [ ] `proto/jaco/v1/commands.proto:219` — add `string host = 4;` to `SubnetAllocate` (keep `cidr = 3`); add `string host = 3;` to `SubnetFree`.
- [ ] Run `make proto` (buf generate) to regenerate `pkg/proto/jaco/v1`.
- [ ] `internal/controlplane/state/subnets.go:13` — include `host` in the store key (separator `\x00` between all three); change `SubnetKey` to `SubnetKey(deployment, network, host string)`.
- [ ] Update every `state.SubnetKey(...)` caller to the 3-arg form — `internal/runtime/reconciler/reconciler.go:42` and `:253` (pass `r.hostname`), `internal/discovery/ipam/ipam.go` (Allocate/Free), plus any others from `git grep -n 'SubnetKey('`.
- [ ] `internal/discovery/ipam/ipam.go:77` — change `Allocate(deployment, network string)` to `Allocate(deployment, network, host string)`; add a `sync.Mutex` field on `IPAM` and guard the read-`nextFree`-apply sequence with it; set `SubnetAllocate.Host`. Stay idempotent on `(deployment, network, host)`.
- [ ] `internal/discovery/ipam/ipam.go:134` — delete `EnsureSubnets`; keep `nextFree` (still `/16` third-octet walk) and `IsExhausted`.
- [ ] `internal/controlplane/fsm/fsm.go:337` — `SubnetAllocate` apply: set `Host: sa.GetHost()` on the stored `Subnet`.
- [ ] `internal/controlplane/grpc/deploy.go:100-114` — remove the apply-time `ipam.New` + `EnsureSubnets` block (incompatible with the host key; lazy per-host allocation lands in task 03). Drop any `deploy_test.go` assertion that apply allocates subnets.

## Acceptance criteria
- [ ] `make proto` exits 0 and `go build ./...` exits 0.
- [ ] `go test ./internal/discovery/ipam/ ./internal/controlplane/state/ ./internal/controlplane/fsm/ -race -count=1` passes (unit: `Allocate(dep,net,host)` idempotency; two hosts of the same `(dep,net)` get distinct `/24`s and distinct keys).
- [ ] `git grep -nE 'func SubnetKey\(deployment, network, host string\)' internal/controlplane/state/subnets.go` matches 1.
- [ ] `git grep -n 'EnsureSubnets' internal/` matches 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
