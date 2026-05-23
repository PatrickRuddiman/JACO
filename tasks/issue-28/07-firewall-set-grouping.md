Parent slice: [datapath](../../slices/issue-28/datapath.md)
Depends on: none

# Task 07 — firewall-set-grouping

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Accept cross-host intra-`(deployment,network)` traffic by grouping all per-host `/24`s of a `(deployment,network)` into one nftables set.

## Tasks
- [ ] `internal/discovery/firewall/render.go:45` (`Render`) — group `in.Subnets` by `SetName(deployment, network)`; emit one `set` per group with `elements = { all CIDRs in the group }` (sorted for determinism); emit one `ip saddr @<set> ip daddr @<set> accept` per group in the `forward` chain (dedupe by `SetName`).
- [ ] `internal/discovery/firewall/testdata/2dep-2net.nft` (and any other golden under `testdata/`) — regenerate/extend so a `(deployment,network)` with two per-host CIDRs renders as a single multi-element set with one forward rule.
- [ ] `internal/discovery/firewall/render_test.go` — add a case asserting two per-host CIDRs for one `(dep,net)` collapse into one set + one forward rule.

## Acceptance criteria
- [ ] `go test ./internal/discovery/firewall/ -race -count=1` passes (unit/golden: multi-CIDR-per-set rendering; no duplicate set names or duplicate forward rules).
- [ ] `git grep -nE 'elements = \{ [0-9].*,' internal/discovery/firewall/testdata/` matches ≥ 1 (a multi-element set in a golden).
- [ ] `go build ./...` exits 0.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
