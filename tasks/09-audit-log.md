Parent slice: [control-plane](../slices/control-plane.md), [cli](../slices/cli.md)
Depends on: 06

# Task 09 — audit-log

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Closed-set audit-event entity, queryable by time/type/follow, with `jaco audit` CLI rendering matching the configured output format.

## Tasks
- [ ] In `proto/jaco/v1/entities.proto`, define `enum AuditEventType` with the closed set: `APPLY, DELETE, ROLLBACK, NODE_JOIN, NODE_REMOVE, TOKEN_ISSUE, TOKEN_REVOKE, CERTIFICATE_ISSUED, CERTIFICATE_RENEWED, CERTIFICATE_FAILED, ISOLATION_RULESET_RECONCILED, ISOLATION_UNAVAILABLE, BACKUP_TAKEN, RESTORE_COMPLETED, UPGRADE_SUCCEEDED, UPGRADE_FAILED`. Regenerate via `make proto`.
- [ ] Ensure FSM `Apply` (from task 04) writes an `AuditEvent{type, identity, payload_summary, raft_index, ts}` for every entity-mutating Command variant.
- [ ] Add `Audit.Query(req{since, until, types[], follow}) returns stream AuditEvent` handler in `internal/controlplane/grpc/audit.go`. Without `follow`: send historical events matching the filter, then close. With `follow`: send historical, then subscribe to `Brokers.AuditEvents()` and stream subsequent matches.
- [ ] Create `cmd/jaco/audit.go` with `--since <duration>` (e.g. `1h`), `--type <comma-list>` (one of the closed set, lowercased), `-f/--follow`. Renders via `RenderTable`/`RenderJSON`/`RenderYAML`.
- [ ] Create `internal/controlplane/grpc/audit_test.go`: bootstrap; apply a no-op token revoke; query with `--type token_revoke`; assert 1 result with the expected identity.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/grpc/... -race -count=1 -run Audit` exits 0.
- [ ] After build, in the 3-node test rig: `./jaco audit --type apply -o json | jq '.[0].type'` prints `"apply"`.
- [ ] `git grep -nE 'AuditEventType' proto/jaco/v1/entities.proto` returns at least 1 match.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
