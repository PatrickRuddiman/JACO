Parent slice: [cli](../slices/cli.md), [control-plane](../slices/control-plane.md)
Depends on: 21, 12

# Task 24 — status-rpc-and-cli

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`Deploy.Status` snapshot + `jaco status [deployment[/service]] -w` watch-stream re-render.

## Tasks
- [x] Add `Deploy.Status(req{deployment_filter, service_filter})` handler in `internal/controlplane/grpc/status.go`. Reads from local state.{Deployments, ReplicasDesired, ReplicasObserved, Routes}; no leader required. Filters observed replicas by joining via state.ReplicasDesired (which carries the deployment + service back-reference).
- [x] Implement the `Watch.Subscribe` server-side handler in `internal/controlplane/grpc/watch.go` that multiplexes per-entity-type subscriptions (deployments / replicas_observed / routes for v1) and filters by deployment when DeploymentFilter is set. Per-replica filtering joins through ReplicasDesired for the deployment match. Register the Watch service in grpcsrv.NewServer.
- [x] Create `cmd/jaco/status.go` registering `jaco status [deployment[/service]] [-w]`. Without -w: one snapshot rendered as 3 cliclient.RenderTable tables (deployments / replicas / routes). With -w: prints the initial snapshot, opens Watch.Subscribe, and on each SubscribeEvent prints `---` then re-fetches via Deploy.Status. The streaming body is extracted into runStatus + runStatusWatch so unit tests inject fake pb.DeployClient + pb.WatchClient.
- [x] Six tests pass with -race. Three server-side integration tests (against the two-node cluster): Status returns Deployment + Routes after Apply; Status filters by deployment name; Watch.Subscribe streams >= 2 events across two Applies (the AC's "at least 2 render snapshots" surface). Three CLI tests: render emits the three table headers + data; server-error propagation; watch-mode produces >= 4 snapshots (1 initial + 3 events) with `---` separators between them.
- [ ] **Deferred**: `scripts/test/status-watch.sh` 2-process E2E — depends on `jaco serve` (not yet wired into a runnable daemon). The Go integration test exercises the same Watch.Subscribe surface end-to-end and asserts the AC ("at least 2 snapshots").

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/... -race -count=1 -run Status` exits 0 (3 server tests).
- [x] `go test ./cmd/jaco/... -race -count=1 -run Status` exits 0 (3 CLI tests).
- [x] Test asserts the captured stdout from the watcher contains at least 2 render snapshots (`TestRunStatusWatch_ReRendersOnEveryEvent` — 4 snapshots and >= 3 separators).
- [ ] `bash scripts/test/status-watch.sh` — deferred to daemon entry.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
