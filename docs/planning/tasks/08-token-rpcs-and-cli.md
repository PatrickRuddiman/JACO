Parent slice: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md)
Depends on: 06

# Task 08 — token-rpcs-and-cli

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement operator token issuance, revocation, and listing — server handlers plus `jaco token` CLI — and assert that revocation propagates cluster-wide within the spec's 5s budget.

## Tasks
- [x] Add `Tokens.Issue(req{identity}) returns (TokenIssueResponse{token, identity, issued_at})` handler in `internal/controlplane/grpc/token.go` (service was renamed from `Token` to `Tokens` in task 01 to avoid the entity-message collision): generate 32 random bytes hex-encoded; SHA-256 hash; raft-Apply `Command{TokenIssue}{identity, hashed_secret}`; return the cleartext token once (annotated as never logged).
- [x] Add `Tokens.Revoke(req{identity}) returns (TokenRevokeResponse{})`: raft-Apply `Command{TokenRevoke}{identity}`. Idempotent — revoking an unknown identity is not an error (closed-set non-existence isn't leaked).
- [x] Add `Tokens.List(req) returns (TokenListResponse{tokens})`: read from local `state.Tokens` and project to `TokenInfo{identity, issued_at, revoked_at}` (hashed_secret stripped at the response shape).
- [x] Register `Tokens` service in `grpcsrv.NewServer` alongside `Cluster`; share `applyRaftCommand` between membership and token handlers.
- [x] Create `cmd/jaco/token.go` with `jaco token issue --name <identity>` (prints cleartext token once), `jaco token revoke <identity>`, `jaco token list`.
- [x] Create `internal/controlplane/grpc/token_test.go`: factor a `setupTwoNodeCluster` helper (bootstraps A, joins B via the real handshake, brings up both nodes' gRPC servers with their per-node CA-signed certs). Then issue an `alice` token on A → wait for replication into B's state → use alice on B to call `Cluster.Status` (success) → revoke alice on A → poll B's `Cluster.Status` with alice's token until `token_revoked` is observed within 5s wall time. Plus a no-raft-wired `raft_unavailable` path, a List smoke-test that asserts the response shape carries no secret field, and an empty-identity validation test.
- [x] Cleanup: drop the redundant `GenerateNodeKeypair` calls in `server_test.go` (noted from task 06).

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/... -race -count=1 -run Token` exits 0.
- [x] Test asserts cross-node revocation completes within 5s wall time (observed ~105ms in practice).
- [x] `git grep -nE 'never logged|hashed_secret' internal/controlplane/grpc/token.go` confirms the cleartext-token-once invariant is annotated.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
