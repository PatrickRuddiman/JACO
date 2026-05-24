Parent slice: [ingress](../slices/ingress.md)
Depends on: 33, 18

# Task 34 — acme-issuance-and-rebuild

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
HTTP-01 challenge token coordination through raft + Caddy config rebuild loop with 200ms debounce + ACME issuance/renewal that survives node failover.

## Tasks
- [x] Create `internal/ingress/challenge/challenge.go` with two halves. `Issuer.Issue(ctx, domain, token, keyAuth)` raft-Applies `Command{ChallengeTokenStore}` with `expires_at = now + TokenTTL (10min)` so every node sees the token via state.ChallengeTokens. `Handler` is an `http.Handler` matching `/.well-known/acme-challenge/<token>` and serving the matching ChallengeToken's `key_auth` (200 + plain text) or 404 — same handler on every node, reads only from state, so any node fielding the upstream's validation request can answer.
- [x] Create `internal/ingress/rebuild/rebuild.go` with `Reloader` driving the debounced Caddy reload loop. Subscribes to Routes / ReplicasObserved / Certs / ChallengeTokens; on any event coalesces for DebounceWindow=200ms then calls the injected `Builder` (production: `config.BuildCaddyConfig`) and `Loader` (production: `caddy.Load(cfg, false)`). Skips the Loader when the new bytes are byte-identical to the previously-loaded config so a no-op reconcile doesn't disturb Caddy. Initial pass fires immediately on `Run()` so the daemon's first config lands at startup.
- [ ] **Deferred**: certmagic OnEvent hooks emitting `CERTIFICATE_RENEWED` / `CERTIFICATE_FAILED` audit events. Needs the caddy/v2 + certmagic imports + the daemon entry to wire the hooks. The storage layer (task 33) already provides the Lock semantics that gate single-flight renewal — the audit emission is a thin layer on top.
- [ ] **Deferred**: Pebble-based integration test in `challenge_test.go` and `scripts/test/ingress-acme.sh` — needs a running Pebble container + caddy/certmagic library imports + the daemon.
- [x] Watch-driven rebuild loop tested against the real broker registry. The `degraded` replica filter falls out of `config.BuildCaddyConfig`'s state-eligibility check from task 32 (test there: `TestBuildCaddyConfig_HealthyTwoOfThree` proves the degraded replica is excluded).
- [x] Fourteen tests pass with -race. Eight challenge: Issuer.Issue persists ChallengeToken with the right expires_at; Issue rejects empty args; Handler serves key_auth for a known token (the AC end-to-end test stand-in); unknown token → 404; expired token → 404; non-challenge path → 404; token-with-slash path-traversal rejected; concurrent reads safe. Six rebuild: load on config change; skip-load on identical config (Loads counter stays at 1 across 5 Rebuild calls); build-error propagation; load-error propagation; debounced burst (5 route writes → 1 debounced rebuild); each subscribed broker (Routes / ReplicasObserved / Certs / ChallengeTokens) triggers a rebuild.

## Acceptance criteria
- [x] `go test ./internal/ingress/challenge/... -race -count=1` exits 0 (8 tests).
- [x] `go test ./internal/ingress/rebuild/... -race -count=1` exits 0 (6 tests).
- [ ] `JACO_ACME_TEST=1 bash scripts/test/ingress-acme.sh` — deferred to daemon entry + Pebble in CI.
- [ ] Test asserts `CERTIFICATE_RENEWED` audit event written on successful renewal — deferred (audit emission hook lands with the certmagic-side wiring).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
