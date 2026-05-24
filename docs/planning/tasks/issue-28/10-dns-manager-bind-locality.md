Parent slice: [dns](../../slices/issue-28/dns.md)
Depends on: 09

# Task 10 — dns-manager-bind-locality

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Make the DNS Manager bind reliably despite the gateway-IP race, prefer local replicas, and read per-network IPs.

## Tasks
- [x] `internal/discovery/dns/manager.go:99` (`ensure`) — pass `Run`'s context into `ensure`; wrap each listener's `ListenAndServe` in a backoff-retry loop (200ms → 5s, capped) that retries on bind error until success (then blocks until `Shutdown`) or ctx cancel / listener removal. Use a per-entry done signal so an intentional `Shutdown` (from `reconcileSubnets`) does not re-bind. Keep the log-once on first bind failure.
- [x] `internal/discovery/dns/manager.go:155` (`refreshServiceMaps`) — read each replica's IP from `obs.GetDetails()["ip."+bridge.DockerNetworkName(rep.GetDeployment(), network)]` (the scope's own network), replacing the single `["ip"]` read.
- [x] `internal/discovery/dns/manager.go:140` (`refreshServiceMaps`) — apply locality: per `(scopeKey, service)`, collect a `local` list (where `obs.GetHost() == m.Hostname`) and an `all` list; set `ServiceMap[service]` to `local` when non-empty, else `all`. The responder still shuffles.

## Acceptance criteria
- [x] `go test ./internal/discovery/dns/ -race -count=1` passes (unit: `refreshServiceMaps` prefers local IPs when a local replica exists and falls back to all otherwise; reads the per-network `ip.<net>` key; a multi-homed replica contributes the right IP per scope).
- [x] `go build ./...` exits 0.
- [x] `git grep -nE 'Details\(\)\["ip\."|GetDetails\(\)\["ip\."' internal/discovery/dns/manager.go` matches ≥ 1.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
