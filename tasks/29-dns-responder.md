Parent slice: [discovery](../slices/discovery.md)
Depends on: 27, 18

# Task 29 — dns-responder

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Per-bridge `miekg/dns` UDP+TCP responder on the bridge gateway IP that resolves `<service>` and `<service>.jaco.local` within its (deployment, network), returns NXDOMAIN otherwise, and forwards external queries.

## Tasks
- [x] Add `github.com/miekg/dns` to `go.mod`.
- [x] Create `internal/discovery/dns/responder.go` defining `Responder` with `New(scope, services, forwarder)`, `SetServices(map)` (atomic snapshot swap), `Services()` (defensive copy), `Scope()`, and `Handle(req *dns.Msg) *dns.Msg` (pure-Go DNS query handler). Query handling: bare `<service>` and `<service>.jaco.local` → A records (randomized order, TTL 5s); unknown in-scope name → NXDOMAIN; external name (any other dotted name) → forwarded via the injected `Forwarder` and answered with the upstream's A records. Non-A query types return NOERROR with empty answer (v1 IPv4-only mesh).
- [ ] **Deferred**: per-bridge UDP+TCP listener on the bridge gateway IP — needs the daemon entry to know which bridges to bind. Handle() is the pure-Go core that the listener will call per packet.
- [ ] **Deferred**: watch-driven reconciler that maintains ServiceMap from ReplicaObserved+ReplicaDesired joins, filtering by state=RUNNING + last_health_at<10s. The Handle layer takes the map already-filtered; the reconciler is the natural follow-up.
- [ ] **Deferred**: `manager.go` spawning one Responder per bridge — lands with the daemon entry.
- [x] Ten unit tests pass with -race: bare-service lookup returns all healthy replica IPs; FQDN `.jaco.local` alias returns the same; unknown in-scope service → NXDOMAIN (the AC); foreign service (not in this responder's ServiceMap) → NXDOMAIN (the AC); external dotted name forwarded to the upstream; forwarder error → NXDOMAIN; non-A query (AAAA) returns empty answer; SetServices does an atomic swap; Services() returns a defensive copy; healthy-only ServiceMap excludes a degraded replica (the AC).

## Acceptance criteria
- [x] `go test ./internal/discovery/dns/... -race -count=1` exits 0 (10 tests).
- [x] Test asserts NXDOMAIN response code for a service not in the bridge's scope (`TestHandle_ServiceNotInScopeReturnsNXDOMAIN`).
- [x] Test asserts the returned A records exclude a `degraded` replica (`TestHandle_HealthyOnlyExcludesDegradedReplicas`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
