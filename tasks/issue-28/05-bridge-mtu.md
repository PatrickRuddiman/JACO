Parent slice: [datapath](../../slices/issue-28/datapath.md)
Depends on: none

# Task 05 — bridge-mtu

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Create per-host bridges at MTU 1420 so WG-routed cross-host packets don't fragment.

## Tasks
- [ ] `internal/discovery/bridge/bridge.go` — add a package constant `BridgeMTU = 1420`.
- [ ] `internal/discovery/bridge/bridge.go:124` — add `"com.docker.network.driver.mtu": "1420"` to the `Options` map passed to `NetworkCreate` in `Ensure` (alongside `com.docker.network.bridge.name`).

## Acceptance criteria
- [ ] `go test ./internal/discovery/bridge/ -race -count=1` passes (unit: a fake docker recording `NetworkCreate` options asserts the mtu option is `"1420"`).
- [ ] `git grep -n 'com.docker.network.driver.mtu' internal/discovery/bridge/bridge.go` matches 1.
- [ ] `go build ./...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
