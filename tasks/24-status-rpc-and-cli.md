Parent slice: [cli](../slices/cli.md), [control-plane](../slices/control-plane.md)
Depends on: 21, 12

# Task 24 — status-rpc-and-cli

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`Deploy.Status` snapshot + `jaco status [deployment[/service]] -w` watch-stream re-render.

## Tasks
- [ ] Add `Deploy.Status(req{deployment_filter, service_filter}) returns (StatusResponse{deployments[], replicas[], routes[]})` handler in `internal/controlplane/grpc/deploy.go`. Reads from local `state.*`; no leader required.
- [ ] Create `cmd/jaco/status.go` registering `jaco status [deployment[/service]] [-w]`. Without `-w`: one snapshot rendered via `RenderTable` (3 sub-tables: deployments, replicas, routes). With `-w`: snapshot, then `Watch.Subscribe(entity_type=deployments + replicas_observed + routes)` filtered to deployment; on each `Event{Updated|Added|Removed}` clear screen (`\x1b[2J\x1b[H` on TTY; newline separator otherwise) and re-render. On `Event{Resync}` re-fetch via `Deploy.Status`.
- [ ] Wire `Watch.Subscribe` to multiplex multiple entity types in a single stream (already supported by the Watch service from task 03; this task adds the client-side fan-in).
- [ ] Create `scripts/test/status-watch.sh` E2E: apply a 2-replica deployment, run `jaco status sample -w &`, sleep 1s, raft-Apply a replica count change to 3, sleep 2s, kill the watcher, assert the captured output contains 2 distinct re-renders (regex matches deployment row at least twice).

## Acceptance criteria
- [ ] `go test ./internal/controlplane/grpc/... -race -count=1 -run Status` exits 0.
- [ ] `bash scripts/test/status-watch.sh` exits 0.
- [ ] Test asserts the captured stdout from the watcher contains at least 2 render snapshots.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
