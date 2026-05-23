Parent slice: [dns](../../slices/issue-28/dns.md)
Depends on: none

# Task 12 — networkconnect-aliases

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Register service aliases on every network attach so Docker's embedded DNS resolves same-host service names even if the JACO responder is down.

## Tasks
- [ ] `internal/runtime/lifecycle/config.go` — add a helper (one func, two callsites) returning `[]string{ spec.Service, spec.Service + "." + spec.Deployment, spec.Service + "." + spec.Deployment + ".jaco.internal" }` from a `compose.ContainerSpec`.
- [ ] `internal/runtime/lifecycle/config.go:58` — set `EndpointsConfig[spec.Networks[0]] = &network.EndpointSettings{Aliases: <helper>(spec)}` (replace the empty `{}`).
- [ ] `internal/runtime/lifecycle/lifecycle.go:120` — pass `&network.EndpointSettings{Aliases: <helper>(spec)}` to `NetworkConnect` (replace `nil`).
- [ ] `internal/runtime/lifecycle/config_test.go:163` and `internal/runtime/lifecycle/lifecycle_test.go:110` — update assertions: create-time `EndpointsConfig` now carries the three aliases; the fake `NetworkConnect` captures and asserts the `EndpointSettings.Aliases`.

## Acceptance criteria
- [ ] `go test ./internal/runtime/lifecycle/ -race -count=1` passes (unit: create-time `EndpointsConfig[net]` carries `[service, service.deployment, service.deployment.jaco.internal]`; the additional-network `NetworkConnect` receives the same aliases).
- [ ] `git grep -nE 'Aliases:' internal/runtime/lifecycle/config.go internal/runtime/lifecycle/lifecycle.go` matches ≥ 2.
- [ ] `go build ./...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
