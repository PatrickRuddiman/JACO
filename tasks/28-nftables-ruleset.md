Parent slice: [discovery](../slices/discovery.md)
Depends on: 25

# Task 28 — nftables-ruleset

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Render the full `inet jaco` ruleset as text and apply atomically via `nft -f`; self-test on boot; refuse `READY=1` to systemd if the table doesn't match expectations.

## Tasks
- [ ] Create `internal/discovery/firewall/render.go` with `Render(input RuleInput) string` where `RuleInput{Subnets []Subnet, WGPort int, GrpcPort int, IngressPorts []int}`. Output matches discovery slice §4 ruleset.
- [ ] Named sets: `set dep_net_<dep>_<net> { type ipv4_addr; flags interval; elements = { <cidr> }; }`. Sanitize set names to `[a-zA-Z0-9_]`; if combined length > 63 chars (nftables identifier limit), hash with SHA-1 prefix.
- [ ] Forward chain: `chain forward { type filter hook forward priority 0; policy drop; }` containing `ct state established,related accept;`, then per-set `ip saddr @<X> ip daddr @<X> accept;`, then implicit DROP.
- [ ] Input chain per slice §4: lo, udp 51820, iifname wg-jaco, jaco-* udp 53, tcp 7000, tcp {80,443}, implicit DROP.
- [ ] Output chain: `policy accept` (no constraints).
- [ ] Create `internal/discovery/firewall/apply.go` with `Apply(ctx, ruleset string) error`. Writes to a temp file (`os.CreateTemp` then `chmod 0600`); invokes `nft -f <file>` via `os/exec`; deletes the file on success or failure.
- [ ] Create `internal/discovery/firewall/selftest.go` with `SelfTest(ctx) error`: runs `nft -j list table inet jaco`; parses JSON; asserts chains `forward`, `input`, `output` exist with expected hook + priority + policy; asserts all expected sets present. On mismatch return `Error{code:"isolation_self_test_failed", details:{missing,extra}}`.
- [ ] Wire into daemon startup: render+apply ruleset before `sd_notify(READY=1)`; if SelfTest fails, audit `ISOLATION_UNAVAILABLE` and exit non-zero (systemd considers the unit failed).
- [ ] Golden-file test in `internal/discovery/firewall/render_test.go`: 2 subnets across 2 deployments → golden output byte-equal to `testdata/firewall/2dep-2net.nft`.

## Acceptance criteria
- [ ] `go test ./internal/discovery/firewall/... -race -count=1` exits 0.
- [ ] Golden-file test asserts byte-equality with the fixture.
- [ ] `git grep -nE 'isolation_self_test_failed' internal/discovery/firewall/` matches.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
