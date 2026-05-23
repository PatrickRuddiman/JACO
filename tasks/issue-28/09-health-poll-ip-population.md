Parent slice: [dns](../../slices/issue-28/dns.md)
Depends on: none

# Task 09 — health-poll-ip-population

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Populate per-network container IPs into `ReplicaObserved.Details` so the DNS responder has IPs to answer with (nothing writes them today).

## Tasks
- [ ] `internal/runtime/health/health.go:185` (in `poll`, after the successful `ContainerInspect`) — for each entry in `info.NetworkSettings.Networks`, write `Details["ip." + <dockerNetworkName>] = <network>.IPAddress` on the `ReplicaObserved`. Initialize `obs.Details` if nil; keep the existing `exit_code` detail at `health.go:199`.

## Acceptance criteria
- [ ] `go test ./internal/runtime/health/ -race -count=1` passes (unit: a fake `ContainerInspect` returning two networks yields two `Details["ip.<net>"]` keys carrying the right IPs; the `exit_code` detail still appears on a non-zero exit).
- [ ] `git grep -nE 'Details\["ip\."' internal/runtime/health/health.go` matches ≥ 1.
- [ ] `go build ./...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
