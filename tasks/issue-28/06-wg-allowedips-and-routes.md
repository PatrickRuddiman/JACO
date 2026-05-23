Parent slice: [datapath](../../slices/issue-28/datapath.md)
Depends on: 00

# Task 06 — wg-allowedips-and-routes

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Carry cross-host container traffic over WireGuard: add each peer's container `/24`s to its `AllowedIPs` and reconcile `dev jaco0` kernel routes for every other host's subnets.

## Tasks
- [ ] `internal/discovery/wgmesh/sync.go:147` (`BuildConfig`) — for each peer, append every `state.Subnets` CIDR where `Subnet.GetHost() == peer.GetHostname()` to that peer's `AllowedIPs` (in addition to the existing `/32`). Keep `ReplaceAllowedIPs: true`.
- [ ] `internal/discovery/wgmesh/sync.go` — add a `routeDiff(desired, current []string) (add, del []string)` pure helper (new or in `sync.go`) so the route reconcile is unit-testable without shelling out.
- [ ] `internal/discovery/wgmesh/sync.go:125` (`Syncer.tick`) — after `ConfigureDevice`, run a route-reconcile step: desired = CIDRs of `state.Subnets` where `GetHost() != s.SelfHostname`; current = parse plain-text `ip route show dev jaco0` (one CIDR per line; no `-j`/JSON — busybox-compatible); apply `routeDiff` via `ip route add <cidr> dev jaco0` / `ip route del <cidr> dev jaco0`. Skip and log-once when `jaco0` is absent (reuse the `loggedConfigError` once-pattern).

## Acceptance criteria
- [ ] `go test ./internal/discovery/wgmesh/ -race -count=1` passes (unit: `BuildConfig` yields a peer whose `AllowedIPs` include its container `/24` plus its `/32`; `routeDiff` returns the correct add/del sets for given desired/current).
- [ ] `go build ./...` exits 0.
- [ ] `git grep -nE 'route show dev jaco0|RouteList' internal/discovery/wgmesh/sync.go` matches the plain-text form (no `-j`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
