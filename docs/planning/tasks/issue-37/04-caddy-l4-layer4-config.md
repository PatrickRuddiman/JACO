Parent slice: [TCP ingress — datapath](../../slices/issue-37/datapath.md)
Depends on: none

# Task 04 — caddy-l4 dependency + BuildCaddyConfig layer4 emission

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Add the caddy-l4 dependency and extend the pure `BuildCaddyConfig` to emit an `apps.layer4` block (one round-robin TCP proxy server per published port over eligible overlay upstreams), omitted entirely when there are no TCP routes so existing golden output is byte-identical.

## Tasks
- [x] Add `github.com/mholt/caddy-l4 v0.1.1` via `go get github.com/mholt/caddy-l4@v0.1.1` (its `go.mod` pins `caddy/v2 v2.11.3`, matching JACO — no other module should move in `go.mod`).
- [x] Register caddy-l4's modules in `internal/daemon/grpc/ingress.go` with a blank import beside the standard-modules import (`ingress.go:18`), so `caddy.Load` resolves the `layer4` app + its `proxy` handler. Confirm the exact import path(s) from the `caddy-l4` module layout.
- [x] In `internal/ingress/config/config.go`, add a `TCPRoute` view type `{ PublishedPort int; Deployment string; Service string; ContainerPort int }` (parallel to `config.Route`).
- [x] Add a `tcpRoutes []TCPRoute` parameter to `BuildCaddyConfig` (`config.go:85`). When `len(tcpRoutes) == 0`, do not add an `apps.layer4` key at all (parallel to the `apps.tls` omission at `config.go:191`).
- [x] When TCP routes exist, emit `apps.layer4.servers` as a map keyed `tcp_<published_port>`: each server `listen: [":<published_port>"]` with one route whose `proxy` handler lists `upstreams` of `{dial: ["<overlayIP>:<containerPort>"]}` for each **eligible** replica of `(deployment, service)` — reuse the existing `healthyByService` + `services` maps and `isEligible` (`config.go:94-107,362`) — and `load_balancing.selection.policy = "round_robin"`. Servers in ascending port order; upstreams sorted by replica id (mirror `config.go:103-106`).
- [x] **Omit zero-upstream routes** (do not emit an empty-upstream server): caddy-l4's `l4proxy.Handler.Provision` errors `no upstreams defined`, which would fail the atomic `caddy.Load`. If no server remains, omit `apps.layer4` entirely.
- [x] Update the existing `BuildCaddyConfig` caller at `internal/daemon/grpc/ingress.go:91` to pass `nil` for `tcpRoutes` (the real projection lands in task 05) and any test callers in `internal/ingress/config/config_test.go`.
- [x] Add golden coverage: a new `internal/ingress/config/testdata/tcp-2of3.golden.json` for a TCP route with 2-of-3 healthy upstreams, plus a config_test case asserting the zero-TCP path still matches the existing goldens byte-for-byte.
- [x] Add a `caddy.Load` integration assertion (follow `internal/ingress/acme_integration_test.go`) that a `BuildCaddyConfig` output containing a layer4 server loads without error.

## Acceptance criteria
- [x] `go test ./internal/ingress/config/...` passes (new TCP golden + unchanged existing goldens).
- [x] `go test ./internal/ingress/ -run Integration` passes — a layer4-bearing config loads via `caddy.Load` with no error.
- [x] `go build ./...` exits 0 and `go vet ./internal/ingress/...` exits 0.
- [x] `git grep -nE 'mholt/caddy-l4 v0\.1\.1' go.mod` matches.
- [x] `test -f internal/ingress/config/testdata/tcp-2of3.golden.json`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
