Parent slice: [runtime](../slices/runtime.md), [cli](../slices/cli.md)
Depends on: 17, 12

# Task 19 — logs-streaming

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`Internal.Logs` peer RPC + `Deploy.Logs` server-side fanout + `jaco logs -f --since` CLI.

## Tasks
- [ ] Create `internal/runtime/logs/logs.go` with `Stream(ctx, replicaID string, since time.Duration, follow bool) (<-chan LogLine, error)`. `LogLine{ReplicaID, Host, Stream ∈ {stdout,stderr}, Ts, Line}`. Use `ContainerLogs(ShowStdout: true, ShowStderr: true, Follow: follow, Since: …)` and `stdcopy.StdCopy` to demux. Ts comes from docker's per-line prefix when `Timestamps: true`.
- [ ] Add `Internal.Logs(req{replica_id, since_seconds, follow}) returns stream LogLine` handler in `internal/controlplane/grpc/internal.go`: wires `runtime/logs.Stream` onto a gRPC streaming response on the node hosting the replica.
- [ ] Add `Deploy.Logs(req{deployment, service, follow, since_seconds}) returns stream LogLine` handler in `internal/controlplane/grpc/deploy.go`: enumerate replicas via `state.ReplicasDesired`, group by host, open `Internal.Logs` peer streams concurrently to each host, merge channels in arrival order, push to the CLI client.
- [ ] Add `cmd/jaco/logs.go` registering `jaco logs <deployment>/<service> [-f] [--since <duration>]`. Default `--since 5m`. Render `[<replica-id>@<host>] <line>` on stdout; flush after each line. Ctrl-C closes the stream cleanly via `ctx.Done()`.
- [ ] Create `scripts/test/logs-fanout.sh` E2E on 2-node rig: deploy 2-replica busybox printing `hello-<replica_id>` on a loop; run `jaco logs sample/echo -f --since 1m` for 5s; assert ≥2 lines per replica observed (`grep -c "hello-sample-echo-0"` and `…-1` both ≥1).

## Acceptance criteria
- [ ] `go test -tags=docker ./internal/runtime/logs/... -race -count=1` exits 0.
- [ ] `bash scripts/test/logs-fanout.sh` exits 0.
- [ ] Test asserts the line render format `[<replica-id>@<host>] ` is present in the CLI output.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
