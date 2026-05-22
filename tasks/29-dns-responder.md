Parent slice: [discovery](../slices/discovery.md)
Depends on: 27, 18

# Task 29 — dns-responder

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Per-bridge `miekg/dns` UDP+TCP responder on the bridge gateway IP that resolves `<service>` and `<service>.jaco.local` within its (deployment, network), returns NXDOMAIN otherwise, and forwards external queries.

## Tasks
- [ ] Add `github.com/miekg/dns` to `go.mod`.
- [ ] Create `internal/discovery/dns/responder.go` defining `type Responder struct { ... }` with `Start(ctx, gatewayIP string, deployment, network string) error` and `Stop(deployment, network string) error`. Binds UDP+TCP `:53` on `gatewayIP`.
- [ ] Cache: subscribe to `ReplicaObserved` watch filtered to services attached to (deployment, network). Maintain in-memory `serviceName → []IP` map updated on watch events.
- [ ] Eligibility: include only replicas where `state == "running"` and `time.Since(last_health_at) < 10s`.
- [ ] Query handler:
  - For `<service>.jaco.local` and bare `<service>` (where bare matches a known service in this scope): return A records, one per healthy replica, randomized order.
  - For services outside this (deployment, network): return NXDOMAIN.
  - For external hostnames (anything else): forward via `net.Resolver` using `/etc/resolv.conf` as upstream.
- [ ] Create `internal/discovery/dns/manager.go`: spawns one `Responder` per bridge present on this node; reconciles on `Subnet` / bridge change events.
- [ ] Unit tests with mocked listeners (use `miekg/dns/dnstest`): positive lookup returns the expected IPs; unhealthy replicas excluded; foreign service returns NXDOMAIN; external query forwarded.

## Acceptance criteria
- [ ] `go test ./internal/discovery/dns/... -race -count=1` exits 0.
- [ ] Test asserts NXDOMAIN response code for a service not in the bridge's (deployment, network) scope.
- [ ] Test asserts the returned A records exclude a `degraded` replica.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
