Parent slice: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md)
Depends on: 06

# Task 08 — token-rpcs-and-cli

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement operator token issuance, revocation, and listing — server handlers plus `jaco token` CLI — and assert that revocation propagates cluster-wide within the spec's 5s budget.

## Tasks
- [ ] Add `Token.Issue(req{identity}) returns (TokenIssueResponse{token, identity, issued_at})` handler in `internal/controlplane/grpc/token.go`: generate 32 random bytes hex-encoded; SHA-256 hash; raft-Apply `Command{TokenIssue}{identity, hashed_secret, issued_at}`; return the cleartext token once (never logged anywhere).
- [ ] Add `Token.Revoke(req{identity}) returns (TokenRevokeResponse{})`: raft-Apply `Command{TokenRevoke}{identity, revoked_at}`.
- [ ] Add `Token.List(req) returns (TokenListResponse{tokens})`: read from local `state.Tokens`; never return `hashed_secret`.
- [ ] Create `cmd/jaco/token.go` with `jaco token issue --name <identity>` (prints cleartext token to stdout once), `jaco token revoke <identity>`, `jaco token list` (table by default; respects `-o`).
- [ ] Create `internal/controlplane/grpc/token_test.go`: 2-node test cluster; issue token on node A; use it on node B (success); revoke on node A; retry on node B in a poll loop with 100ms cadence; assert revocation observed (`token_revoked`) within 5s wall time.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/grpc/... -race -count=1 -run Token` exits 0.
- [ ] Test asserts cross-node revocation completes within 5s wall time (deadline failure if not).
- [ ] `git grep -nE 'never logged|hashed_secret' internal/controlplane/grpc/token.go` confirms the cleartext-token-once invariant is annotated.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
