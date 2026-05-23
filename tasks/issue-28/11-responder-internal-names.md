Parent slice: [dns](../../slices/issue-28/dns.md)
Depends on: none

# Task 11 — responder-internal-names

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Resolve `.jaco.internal` and `<service>.<deployment>` names in the per-bridge responder (the `.local` suffix is mDNS-reserved).

## Tasks
- [x] `internal/discovery/dns/responder.go:175` (`parseInScopeName`) — replace the `.jaco.local` suffix with `.jaco.internal`; treat as in-scope (resolving to `<service>`): bare `<service>`, `<service>.<deployment>` when `deployment == r.scope.Deployment`, `<service>.jaco.internal`, and `<service>.<deployment>.jaco.internal`. Any other dotted name stays external (forwarded).
- [x] `internal/discovery/dns/responder.go:8` — update the package doc comment that references `<service>.jaco.local`.
- [x] `internal/discovery/dns/responder_test.go` — switch suffix expectations to `.jaco.internal`; add cases for `<service>.<deployment>`, `<service>.jaco.internal`, `<service>.<deployment>.jaco.internal`, and an out-of-scope dotted name that forwards.

## Acceptance criteria
- [x] `go test ./internal/discovery/dns/ -race -count=1` passes (unit: all four in-scope name forms resolve to the service IPs; an out-of-scope dotted name is forwarded).
- [x] `git grep -n 'jaco.local' internal/discovery/dns/` matches 0.
- [x] `go build ./...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
