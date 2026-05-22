Parent slice: [cli](../slices/cli.md)
Depends on: 12, 14

# Task 15 — deploy-cli-subcommands

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`jaco apply` (with `--dry-run`), `jaco rollback`, `jaco delete` subcommands.

## Tasks
- [ ] Create `cmd/jaco/apply.go` registering `jaco apply <jaco.yaml> [--dry-run]`. Read jaco.yaml + any `compose: <path>` reference relative to the jaco.yaml directory; pass bytes to `Deploy.Apply`. With `--dry-run`: render `Diff` via `RenderTable` grouped by entity type (header rows: `Replicas (+N -M ~K)`, `Routes (...)`, `Subnets (...)`); print `No changes` when diff is empty. Without `--dry-run`: print `Applied revision: <N>` on success.
- [ ] Create `cmd/jaco/rollback.go` registering `jaco rollback <deployment>`; calls `Deploy.Rollback`; prints `Rolled back to revision: <N>`.
- [ ] Create `cmd/jaco/delete.go` registering `jaco delete <deployment>`; calls `Deploy.Delete`; prints `Deleted deployment: <name>`.
- [ ] Add `testdata/sample.jaco.yaml` and `testdata/sample.compose.yml` exercising every honored compose field plus 2-replica spread placement.
- [ ] Create `scripts/test/apply-deploy.sh` E2E: bootstrap → `jaco apply testdata/sample.jaco.yaml --dry-run` exits 0; without dry-run prints `Applied revision`; `jaco delete sample` exits 0.

## Acceptance criteria
- [ ] `bash scripts/test/apply-deploy.sh` exits 0.
- [ ] `./jaco apply testdata/sample.jaco.yaml --dry-run` exits 0 and stdout matches one of `^No changes$` or contains `Replicas`.
- [ ] `git grep -nE '^Applied revision:' cmd/jaco/apply.go` matches.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
