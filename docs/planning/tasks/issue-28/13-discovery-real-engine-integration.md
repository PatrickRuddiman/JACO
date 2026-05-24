Parent slice: [control-plane](../../slices/issue-28/control-plane.md), [datapath](../../slices/issue-28/datapath.md), [dns](../../slices/issue-28/dns.md)
Depends on: 03, 05, 07, 10, 11, 12

# Task 13 — discovery-real-engine-integration

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Prove the discovery path end-to-end on a single host with a real Docker engine: a replica gets a per-host `/24`, its bridge is MTU 1420, and the responder answers the service name. (Cross-host routing, AllowedIPs, and SNAT — tasks 06/08 — are exercised by the privileged 3-node isolation rig, not here.)

## Tasks
- [x] Add `internal/discovery/discovery_integration_test.go` with `//go:build docker` (mirror the harness in `internal/runtime/health/health_integration_test.go`). Stand up a single-replica deployment against the real engine: allocate its `(deployment, network, host)` subnet via the leader allocation path, `bridge.Ensure` with the returned CIDR, start a container on the bridge, and feed its observed per-network IP into a DNS Manager responder for that scope.
- [x] In the same test, assert: the `(deployment, network, host)` subnet is present in `state.Subnets`; the created docker network's options carry MTU `1420`; a type-A query for `<service>` against the bridge gateway returns the container's IP; a query for `<service>.<deployment>.jaco.internal` returns the same IP.

## Acceptance criteria
- [x] `go test -tags docker -run TestDiscovery ./internal/discovery/` passes on a host with Docker (+ `CAP_NET_BIND_SERVICE` for the responder bind).
- [x] `go vet -tags docker ./internal/discovery/...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
