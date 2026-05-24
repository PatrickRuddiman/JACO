Parent slice: [cli](../slices/cli.md)
Depends on: 12, 14

# Task 15 — deploy-cli-subcommands

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`jaco apply` (with `--dry-run`), `jaco rollback`, `jaco delete` subcommands.

## Tasks
- [x] Create `cmd/jaco/apply.go` registering `jaco apply <jaco.yaml> [--dry-run]`. Reads jaco.yaml + a compose file (auto-discovered as `compose.yml` / `compose.yaml` next to jaco.yaml; `--compose <path>` overrides), passes bytes to `Deploy.Apply`. With `--dry-run`: prints `No changes` on empty diff, otherwise per-entry `+`/`~`/`-` lines (table-style grouping by entity type lands once task 14's computeDiff is non-empty in task 21/25). Without `--dry-run`: prints `Applied revision: <N>`.
- [x] Create `cmd/jaco/rollback.go` registering `jaco rollback <deployment>`; calls `Deploy.Rollback`; prints `Rolled back to revision: <N>`.
- [x] Create `cmd/jaco/delete.go` registering `jaco delete <deployment>`; calls `Deploy.Delete`; prints `Deleted deployment: <name>`.
- [x] Add `cmd/jaco/testdata/sample.jaco.yaml` + `cmd/jaco/testdata/compose.yml` — the apply test asserts the pair loads via auto-discovery.
- [x] Extract `runApply` / `runRollback` / `runDelete` from the cobra handlers so unit tests can inject a `pb.DeployClient` without spinning up a real gRPC server. Cobra wrappers just resolve flags and call through.
- [x] Ten unit tests in `cmd/jaco/apply_test.go`: dry-run prints `No changes` on empty diff; dry-run renders +/~/- lines for a populated diff; non-dry-run prints `Applied revision: 3`; server errors bubble up; rollback prints `Rolled back to revision: N`; delete prints `Deleted deployment: <name>`; manifest auto-discovery finds `compose.yml`; explicit `--compose` override beats discovery; missing compose file errors with `no compose file found`; sample fixtures parse via the discovery path.
- [ ] `scripts/test/apply-deploy.sh` E2E is **deferred** to task 17 (jaco serve) — the shell test needs a running daemon to dial. The Go unit tests above cover the same surface against an injected fake client.

## Acceptance criteria
- [x] `go test ./cmd/jaco/... -race -count=1` exits 0 (10 tests cover apply/rollback/delete + manifest discovery).
- [x] `runApply` with `dry_run=true` + empty Diff prints `No changes` (asserted by test).
- [x] `git grep -nE 'Applied revision' cmd/jaco/apply.go` matches.
- [ ] `bash scripts/test/apply-deploy.sh` — deferred to task 17.
- [ ] `./jaco apply testdata/sample.jaco.yaml --dry-run` — deferred to task 17.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
