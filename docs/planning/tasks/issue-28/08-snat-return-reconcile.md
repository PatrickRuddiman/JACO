Parent slice: [datapath](../../slices/issue-28/datapath.md)
Depends on: 07

# Task 08 — snat-return-reconcile

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Keep cross-host container source IPs intact by reconciling a `nat POSTROUTING` RETURN for intra-pool traffic on the firewall tick. (Conceptually gated on the cross-host masquerade audit, but the reconcile is idempotent and harmless if Docker doesn't masquerade.)

## Tasks
- [x] `internal/discovery/firewall/` — add a pure helper `snatExemptRule(pool string) string` that renders the RETURN rule matching `saddr <pool> daddr <pool>` for the top of the iptables-compat `nat POSTROUTING` chain, so the rule text is unit-testable.
- [x] `internal/discovery/firewall/reconcile.go:46` (`Tick`) — after the `inet jaco` SelfTest/apply, assert the SNAT RETURN: check whether the rule is present at the top of `nat POSTROUTING` and re-insert via the existing `Applier` if missing. Track this independently of the `inet jaco` SelfTest verdict (a missing RETURN is not an `inet jaco` drift).
- [x] `internal/daemon/grpc/server.go:476` (firewall `Render`/reconciler wiring) — pass the configured pool (`s.ipamPool`, plumbed in task 02; fall back to `ipam.DefaultPoolCIDR`) into the SNAT assertion.

## Acceptance criteria
- [x] `go test ./internal/discovery/firewall/ -race -count=1` passes (unit: `snatExemptRule("10.244.0.0/16")` text is correct; `Tick` invokes the SNAT assert via a recording fake `Applier` when the rule is absent and not when present).
- [x] `go build ./...` exits 0.
- [x] `git grep -n 'snatExemptRule' internal/discovery/firewall/` matches ≥ 2.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
