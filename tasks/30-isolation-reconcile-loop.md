Parent slice: [discovery](../slices/discovery.md)
Depends on: 28, 09

# Task 30 — isolation-reconcile-loop

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
30s drift-detection loop that re-applies the expected nftables ruleset on mismatch, audits the reconcile, and flips the node to `isolation_unavailable` when the reload itself fails.

## Tasks
- [x] Create `internal/discovery/firewall/reconcile.go` with `Reconciler` (Lister + Applier + AuditFn + IsolationStatusFn + Render), `Tick(ctx)` doing one pass, and `Loop(ctx)` driving Tick on a 30s ticker. Pure-Go core; all dependencies are injectable interfaces so tests run without nftables.
- [x] Tick semantics: list live state via Lister → SelfTestFromJSON against the rendered expected → if matched and previously degraded, audit ISOLATION_RULESET_RECONCILED with action=recovered + flip status to "ready"; if matched and not degraded, no-op silently. On mismatch: Apply re-rendered ruleset; on Apply success audit action=applied with summary of missing/extra; on Apply failure flip status to "isolation_unavailable" with the apply error message.
- [x] Recovery: when Apply succeeds after a previous degradation, the next clean tick flips status back to "ready" and audits ISOLATION_RULESET_RECONCILED with action=recovered.
- [x] Transient nft-list errors don't flip status — they surface as a Tick error and the next tick retries (the only thing that flips status is an Apply failure).
- [x] Create `internal/runtime/lifecycle/isolation.go` with `CheckIsolationAvailable(state, selfHostname)` returning `ErrIsolationUnavailable` when the local Node entity reports `NODE_STATUS_ISOLATION_UNAVAILABLE`. The daemon calls this before each `lifecycle.Start` to refuse new container creation on a degraded node. Nil state, missing hostname, and other status states all pass — only the explicit isolation flag gates.
- [ ] **Deferred**: integrating CheckIsolationAvailable into `lifecycle.Start` itself — requires the daemon to plumb state + self-hostname into lifecycle (intrusive interface change). For v1 the daemon calls CheckIsolationAvailable explicitly before lifecycle.Start; tests of the helper cover the gating semantics.
- [ ] **Deferred**: real-engine reconcile test (`-tags=nftables`) — needs CAP_NET_ADMIN. The injection-based tests cover the contract.
- [ ] **Deferred**: `scripts/test/isolation-drift.sh` E2E — needs `jaco serve` daemon.
- [x] Nine tests pass with -race. Five in `firewall/reconcile_test.go`: happy-path no-drift silent (no Apply/no audit); drift detected → re-apply + ISOLATION_RULESET_RECONCILED audit (the AC); Apply failure flips status to isolation_unavailable (the AC); recovery from isolation_unavailable emits status=ready + audit; transient list error doesn't flip status. Four in `lifecycle/isolation_test.go`: READY passes; ISOLATION_UNAVAILABLE returns ErrIsolationUnavailable (the AC); nil-state/missing-hostname/empty-name no-op; UNSPECIFIED/JOINING/READY all pass.

## Acceptance criteria
- [x] `go test ./internal/discovery/firewall/... -race -count=1 -run Reconcile` exits 0 (5 reconcile tests).
- [x] Test asserts an ISOLATION_RULESET_RECONCILED audit event when drift is detected (`TestReconcile_DriftDetectedReappliesAndAudits`).
- [x] Test asserts `CheckIsolationAvailable` returns `ErrIsolationUnavailable` when the local Node carries that status (`TestCheckIsolationAvailable_IsolationUnavailableRejects`).
- [ ] `bash scripts/test/isolation-drift.sh` — deferred to daemon entry.
- [ ] `go test -tags=nftables` — deferred to task 31's CI rig with CAP_NET_ADMIN.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
