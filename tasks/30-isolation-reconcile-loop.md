Parent slice: [discovery](../slices/discovery.md)
Depends on: 28, 09

# Task 30 — isolation-reconcile-loop

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
30s drift-detection loop that re-applies the expected nftables ruleset on mismatch, audits the reconcile, and flips the node to `isolation_unavailable` when the reload itself fails.

## Tasks
- [ ] Create `internal/discovery/firewall/reconcile.go` with `Reconcile(ctx, expected string) error` and a `Loop(ctx, renderInput func() RuleInput)` ticker (30s).
- [ ] On each tick: `nft -j list table inet jaco`, diff structurally against expected output of `Render(renderInput())`. On mismatch:
  - Re-render expected ruleset; call `firewall.Apply` (atomic `nft -f`).
  - raft-Apply `Command{AuditAppend}{type: ISOLATION_RULESET_RECONCILED, identity:"system", payload:{node, diff_summary}}`.
- [ ] If `firewall.Apply` returns an error: raft-Apply `Command{NodeStatusUpdate}{hostname, status:"isolation_unavailable"}`. Existing containers continue running; no new containers may be created (runtime/lifecycle.Start checks this status and returns `Error{code:"isolation_unavailable"}`).
- [ ] On the next successful reload: raft-Apply `Command{NodeStatusUpdate}{hostname, status:"ready"}` and audit `ISOLATION_RULESET_RECONCILED`.
- [ ] Add `Error.code = "isolation_unavailable"` handling in `internal/runtime/lifecycle/lifecycle.go::Start`: refuse to call `ContainerCreate` when local Node entity has `status == "isolation_unavailable"`.
- [ ] Test in `internal/discovery/firewall/reconcile_test.go` (build tag `nftables`, requires CAP_NET_ADMIN): seed expected; shell-call `nft flush table inet jaco`; assert reconcile re-applies within 30s and an audit event is written.
- [ ] E2E `scripts/test/isolation-drift.sh`.

## Acceptance criteria
- [ ] `go test -tags=nftables ./internal/discovery/firewall/... -race -count=1 -run Reconcile` exits 0 (or skipped if nftables unavailable).
- [ ] `bash scripts/test/isolation-drift.sh` asserts an `ISOLATION_RULESET_RECONCILED` audit event present after manual flush.
- [ ] Test asserts runtime/lifecycle.Start returns `Error.code == "isolation_unavailable"` when Node status is set accordingly.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
