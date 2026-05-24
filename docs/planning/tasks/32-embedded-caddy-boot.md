Parent slice: [ingress](../slices/ingress.md)
Depends on: 04, 14

# Task 32 — embedded-caddy-boot

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Embed Caddy v2 as a Go library; load an initial config built from Routes + ReplicaObserved via a pure `BuildCaddyConfig` function; listen on `:80, :443`.

## Tasks
- [x] Create `internal/ingress/config/config.go` with `BuildCaddyConfig(routes, replicas, services, opts)` pure-Go function returning indented JSON. Single `apps.http.servers.jaco` server listening `:80, :443`; per-route reverse_proxy handler with `load_balancing.selection_policy.policy="random"` + 2 retries + 10s passive fail_duration. Upstreams = `{dial: "<replica_ip>:<route.port>"}` for healthy replicas (`state=="running"` AND `now-last_health_at < HealthFreshness=10s`). Routes are sorted by domain for deterministic golden-file output; replicas within a service sorted by id.
- [x] Per-route TLS policy emitted when `TLSAuto=true`: `{subjects:[domain], issuers:[{module:"acme", ca, email?, challenges:{http:{}}}], key_type:"p256", storage:{module:"jaco"}}`. `tls: off` routes omit ACME entirely. ACMEEmail blank-allowed (operator opts-out). `apps.tls` is absent when no routes are TLS-auto.
- [x] Static fallback route at the tail of the routes list: `static_response{status_code:404, headers:{Server:[jaco]}}`. Always present, even with zero declared routes.
- [x] Golden fixtures under `internal/ingress/config/testdata/`: `healthy-2of3.golden.json` (1 route + 3 replicas, 2 healthy + 1 degraded), `tls-off.golden.json` (route with TLSAuto=false), `zero-routes.golden.json` (just the fallback).
- [x] Seven tests pass with -race: healthy-2of3 asserts exactly 2 upstream entries (the AC); tls-off omits the apps.tls section + matches golden; zero-routes produces fallback-only matching golden; stale-health replica excluded; missing-service-meta produces empty upstreams; routes sorted by domain; ACME policy carries key_type=p256, storage.module=jaco, issuer module=acme + email + CA URL.
- [ ] **Deferred**: `internal/ingress/ingress.go` Ingress runner that opens watches + calls `caddy.Load(configBytes, false)` — needs `github.com/caddyserver/caddy/v2` + `certmagic` dependencies and the daemon entry. The config builder is the library piece; the runner is the daemon wiring.

## Acceptance criteria
- [x] `go test ./internal/ingress/config/... -race -count=1` exits 0 (7 tests).
- [x] Test (a) asserts exactly 2 entries in the route's upstreams list (`TestBuildCaddyConfig_HealthyTwoOfThree`).
- [x] `grep -nE '"random"' internal/ingress/config/testdata/healthy-2of3.golden.json` matches.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
