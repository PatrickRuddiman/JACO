Parent slice: [ingress](../slices/ingress.md)
Depends on: 33, 18

# Task 34 — acme-issuance-and-rebuild

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
HTTP-01 challenge token coordination through raft + Caddy config rebuild loop with 200ms debounce + ACME issuance/renewal that survives node failover.

## Tasks
- [ ] Create `internal/ingress/challenge/challenge.go`: when certmagic asks Caddy to serve a challenge, write `ChallengeToken{domain, token, key_auth, expires_at: now+10min}` to raft via `Command{ChallengeTokenStore}`. Implement Caddy HTTP handler matching `/.well-known/acme-challenge/<token>` paths; the handler on every node reads the local watch-fed cache and returns `key_auth` for the matching token (200 + plain text body) or 404.
- [ ] Wire the rebuild loop in `internal/ingress/ingress.go`: 200ms debounce on Route, ReplicaObserved, Cert, ChallengeToken events; recompute config via `config.BuildCaddyConfig`; deep-compare against the previously loaded config (skip if byte-identical); else `caddy.Load(newConfig, false)`.
- [ ] Renewal: certmagic's built-in scheduler runs on every node; `storage.Lock` ensures only one node performs the renewal for a given domain (others observe the new cert via watch within seconds). On renewal success: raft-Apply `Command{AuditAppend}{type: CERTIFICATE_RENEWED, payload:{domain}}`. On renewal failure with 1h backoff cap: raft-Apply `Command{AuditAppend}{type: CERTIFICATE_FAILED, payload:{domain, error}}`; existing cert continues to serve until expiry.
- [ ] Wire `Command{ReplicaCommand}{op: remove_from_routing}` consumption: when a `degraded` replica is observed, exclude it from upstream eligibility (this falls out naturally from the `state == running` AND `last_health_at < 10s` filter; this task adds a regression test).
- [ ] Integration test `internal/ingress/challenge/challenge_test.go` using Pebble (Let's Encrypt staging-like ACME server) + a local DNS hijack (Caddy listens on a non-privileged port for the test): apply a `Route{domain: "test.local", tls: auto}`; assert cert issued and stored in raft within 60s; HTTPS request to the listener succeeds.
- [ ] Add E2E `scripts/test/ingress-acme.sh` covering the Pebble path; gated behind `JACO_ACME_TEST=1` env (so day-to-day CI can skip the heavier rig).

## Acceptance criteria
- [ ] `go test ./internal/ingress/challenge/... -race -count=1` exits 0.
- [ ] `JACO_ACME_TEST=1 bash scripts/test/ingress-acme.sh` exits 0; cert appears in `state.Certs` within 60s.
- [ ] Test asserts `CERTIFICATE_RENEWED` audit event written on successful renewal.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
