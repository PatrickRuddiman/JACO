Parent slice: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md)
Depends on: 06

# Task 09 — audit-log

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Closed-set audit-event entity, queryable by time/type/follow, with `jaco audit` CLI rendering matching the configured output format.

## Tasks
- [x] `enum AuditEventType` already defined in `proto/jaco/v1/entities.proto` (task 01). Closed set: `APPLY, DELETE, ROLLBACK, NODE_JOIN, NODE_REMOVE, TOKEN_ISSUE, TOKEN_REVOKE, CERTIFICATE_*, ISOLATION_RULESET_RECONCILED, ISOLATION_UNAVAILABLE, BACKUP_TAKEN, RESTORE_COMPLETED, UPGRADE_SUCCEEDED, UPGRADE_FAILED, ROLLOUT_INVARIANT_HOLD`.
- [x] FSM `Apply` (task 04) already writes `AuditEvent{type, identity, raft_index, ts, payload}` for every mutation in the closed set.
- [x] Add `Audit.Query(req{since, until, types[], follow}) returns stream AuditEvent` handler in `internal/controlplane/grpc/audit.go`. Without `follow`: send historical events matching the filter, then close. With `follow`: subscribe to `Brokers.AuditEvents` BEFORE reading historical (so events landing mid-snapshot aren't lost), dedupe by raft_index, then stream new matches. KindResync re-fetches state and forwards past `lastIdx`.
- [x] Add `grpcsrv.Options.Brokers *watch.Registry` (passed by the daemon entrypoint or test harness) so the audit server can subscribe.
- [x] Add `grpcsrv.AuditTypeToString` / `grpcsrv.ParseAuditType` helpers — strip the `AUDIT_EVENT_TYPE_` prefix and lowercase. Shared between server-side rendering and the CLI's `--type` parsing.
- [x] Create `cmd/jaco/audit.go` with `--since <duration>` (`time.ParseDuration`), `--type <comma-list>` (lowercased closed-set names), `-f/--follow`. Output respects the global `-o`: JSON array for non-follow, NDJSON for follow, plain table otherwise. (yaml renderer is task 12.)
- [x] Create `internal/controlplane/grpc/audit_test.go`: bootstrap two-node cluster via the shared harness; revoke a token to generate a TOKEN_REVOKE event; query with `Types=[TOKEN_REVOKE]` and assert every returned event is of that type with the expected payload. Plus `Since` cutoff filter test, follow-mode test that asserts a newly-issued token's TOKEN_ISSUE event arrives on the stream within 3s, and a round-trip test for the AuditTypeToString/ParseAuditType helpers.

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/... -race -count=1 -run Audit` exits 0.
- [ ] **3-node test rig E2E**: `./jaco audit --type apply -o json | jq '.[0].type'` prints `"apply"`. Deferred to task 17 (jaco serve daemon entry); the Go integration test exercises the same shape via in-process raft daemons.
- [x] `git grep -nE 'AuditEventType' proto/jaco/v1/entities.proto` returns at least 1 match.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
