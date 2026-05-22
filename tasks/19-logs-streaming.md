Parent slice: [runtime](../slices/runtime.md), [cli](../slices/cli.md)
Depends on: 17, 12

# Task 19 — logs-streaming

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`Internal.Logs` peer RPC + `Deploy.Logs` server-side fanout + `jaco logs -f --since` CLI.

## Tasks
- [x] Create `internal/runtime/logs/logs.go` with `Stream(ctx, d, replicaID, containerID, host, opts) (<-chan *pb.LogLine, error)`. Uses `ContainerLogs(ShowStdout, ShowStderr, Follow, Since, Timestamps)` and `stdcopy.StdCopy` to demux. Ts is parsed from docker's per-line `RFC3339Nano ` prefix; the originating replica id + host get stamped onto every LogLine.
- [x] Add `replica_id` to the `LogsRequest` proto (filled by Internal.Logs callers; Deploy.Logs leaves it empty).
- [x] Add `cmd/jaco/logs.go` registering `jaco logs <deployment>/<service> [-f] [--since <duration>]`. Default `--since 5m`. Renders `[<replica-id>@<host>] <line>` per line; the body is extracted into `runLogs(ctx, client, deployment, service, follow, sinceSeconds, out)` so tests can inject a fake `pb.DeployClient`.
- [x] Stub `Deploy.Logs` server-side: returns `codes.Unimplemented / logs_unimplemented` until the daemon entry can wire `*dockerx.Client` + the local hostname into `grpcsrv.Options`. Documented in deploy.go.
- [ ] **Deferred**: `Internal.Logs` peer-RPC handler + Deploy.Logs cross-node fanout (group replicas by host, dial peer Internal.Logs, merge by arrival). Needs the daemon-side docker wiring described above.
- [ ] **Deferred**: `scripts/test/logs-fanout.sh` 2-node E2E — depends on `jaco serve` + the docker wiring above. The Go integration test surface I CAN cover (runtime/logs.Stream + the CLI rendering) is in place; the cross-node fanout lands once the daemon entry comes together.
- [x] Six runtime/logs tests pass with -race: demuxes stdout / stderr in the docker frame format, stamps replica_id + host, handles a trailing partial line without a newline (flush path), context cancellation closes the channel, propagates ContainerLogs errors, Since duration translates to docker's `since` RFC3339 option.
- [x] Four CLI tests pass with -race: render formats `[<replica-id>@<host>] <line>` (the AC); server errors bubble through `runLogs`; `splitDeploymentService` parses `<dep>/<svc>` and rejects malformed inputs; `parseSinceSeconds` parses `1h`/`30m`/garbage correctly.

## Acceptance criteria
- [x] `go test ./internal/runtime/logs/... -race -count=1` exits 0 (6 pure-Go tests pass).
- [x] Test asserts the `[<replica-id>@<host>] ` render format is present in the CLI output (`TestRunLogs_FormatsLineAsReplicaAtHost`).
- [ ] `go test -tags=docker ./internal/runtime/logs/... -race -count=1` against a real docker engine — deferred to task 31's CI rig.
- [ ] `bash scripts/test/logs-fanout.sh` — deferred (needs daemon-side docker wiring + jaco serve).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
