Parent slice: [discovery](../slices/discovery.md)
Depends on: 25

# Task 28 — nftables-ruleset

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Render the full `inet jaco` ruleset as text and apply atomically via `nft -f`; self-test on boot; refuse `READY=1` to systemd if the table doesn't match expectations.

## Tasks
- [x] Create `internal/discovery/firewall/render.go` with `Render(in RuleInput) string` matching the discovery slice §4 ruleset (table inet jaco; per-subnet named set + ip saddr @X ip daddr @X accept; input chain with the lo / WG / wg-jaco / DNS / GRPC / ingress accepts; output policy accept).
- [x] `SetName(deployment, network)` sanitizes to `[a-zA-Z0-9_]`, defaults empty network to `_default`, and hashes via SHA-1 when the joined identifier would exceed nftables' 63-char limit while preserving the `dep_net_` prefix.
- [x] Forward chain emits `ct state established,related accept` then per-set saddr+daddr accept; input chain emits lo accept, ct state established,related accept, udp dport <WGPort> accept, iifname wg-jaco accept, iifname jaco-* udp dport 53 accept, tcp dport <GrpcPort> accept, ingress ports list (single port without braces, multiple as `{ … }`), final drop; output policy accept.
- [x] Create `internal/discovery/firewall/apply.go` with `Applier` interface + `DefaultApplier()`. Apply writes the ruleset to a 0600 temp file, invokes `nft -f <file>`, and unlinks on success or failure. `IsAvailable()` returns the sentinel `ErrNftNotFound` when `nft` isn't on PATH so the daemon can run in degraded mode.
- [x] Create `internal/discovery/firewall/selftest.go` with `SelfTest(ctx, expected)` + `SelfTestFromJSON(json, expected)`. Runs `nft -j list table inet jaco`, parses the JSON envelope, asserts forward/input/output chains present with hook + policy + priority correct, asserts every expected set present, flags any extra sets. Returns `*SelfTestError{Code:"isolation_self_test_failed", Missing, Extra}` on mismatch.
- [ ] **Deferred**: daemon startup wiring (render+apply before sd_notify(READY=1), audit ISOLATION_UNAVAILABLE on SelfTest failure) — lands with the daemon entry.
- [x] Twelve tests pass with -race. Golden-file test (`TestRender_GoldenTwoDepsTwoNets`) byte-equal to `testdata/2dep-2net.nft` (the AC); deterministic ordering of subnets; SetName sanitization + 63-char fit + hashing when too long + `_default` fallback; ruleset contains every expected chain/port/set element; single ingress port renders without braces; SelfTestFromJSON happy path, missing-chain rejection, extra-set rejection; Apply fails when nft missing; IsAvailable returns nil-or-sentinel.

## Acceptance criteria
- [x] `go test ./internal/discovery/firewall/... -race -count=1` exits 0 (12 tests).
- [x] Golden-file test asserts byte-equality with the fixture (`TestRender_GoldenTwoDepsTwoNets`).
- [x] `git grep -nE 'isolation_self_test_failed' internal/discovery/firewall/` matches (`selftest.go` + the SelfTestError code assertion in `firewall_test.go`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
