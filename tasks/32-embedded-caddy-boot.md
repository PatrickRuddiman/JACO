Parent slice: [ingress](../slices/ingress.md)
Depends on: 04, 14

# Task 32 — embedded-caddy-boot

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Embed Caddy v2 as a Go library; load an initial config built from Routes + ReplicaObserved via a pure `BuildCaddyConfig` function; listen on `:80, :443`.

## Tasks
- [ ] Add `github.com/caddyserver/caddy/v2` and `github.com/caddyserver/certmagic` to `go.mod`.
- [ ] Create `internal/ingress/ingress.go` with `type Ingress struct { ... }` started in `jaco serve`. Opens watches on Routes, ReplicaObserved (filtered to services referenced by Routes), Certs, ChallengeTokens. After catch-up phase, computes initial config and calls `caddy.Load(configBytes, false)`.
- [ ] Create `internal/ingress/config/config.go` with `BuildCaddyConfig(routes []Route, replicas []ReplicaObserved, services map[string]ServiceMeta) []byte`. Output is Caddy JSON config matching ingress slice §4: single `apps.http.servers.jaco` server listening `:80, :443`; one route per Route with host header matcher; upstreams = `{dial: "<replica.host_ip>:<route.port>"}` for healthy replicas (`state == "running"` AND `now - last_health_at < 10s`); LB `{selection_policy: {policy:"random"}, retries: 2, fail_duration: "10s"}`.
- [ ] TLS automation policy: `{on_demand: false, issuers: [{module:"acme", ca:"https://acme-v02.api.letsencrypt.org/directory", email:"<operator>", challenges:{http:{}}}], key_type:"p256", storage:{module:"jaco"}}`. Operator email read from daemon flag `--acme-email`; empty allowed.
- [ ] HTTP-only routes (`tls: off`): no HTTPS listener for that domain.
- [ ] Static fallback route at index N+1: returns 404 with `Server: jaco` header.
- [ ] Unit tests: golden-file fixtures under `internal/ingress/config/testdata/` for (a) 1 route + 3 replicas (2 healthy, 1 degraded) → expected JSON excludes the degraded upstream; (b) `tls: off` route excludes ACME automation; (c) zero routes → fallback-only config.

## Acceptance criteria
- [ ] `go test ./internal/ingress/config/... -race -count=1` exits 0.
- [ ] Golden-file test (a) asserts exactly 2 entries in the route's upstreams list.
- [ ] `grep -nE '"random"' internal/ingress/config/testdata/healthy-2of3.golden.json` matches.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
