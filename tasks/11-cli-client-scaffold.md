Parent slice: [cli](../slices/cli.md)
Depends on: 06

# Task 11 — cli-client-scaffold

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`~/.config/jaco/clusters.yaml` loader with env overrides plus a reusable gRPC client that rotates through `server_addrs` on connection or `no_leader` errors.

## Tasks
- [ ] Create `internal/cliclient/context.go` defining `type Clusters struct { CurrentContext string; Contexts []Context }`, `type Context struct { Name string; ServerAddrs []string; CACertPath string; Token string }`. Loader checks the file is regular and mode is `0600`; on mismatch returns an error with the actual mode in the message.
- [ ] In `internal/cliclient/context.go`, add `Resolve() (*Context, error)`: load YAML; pick `CurrentContext` (or `JACO_CONTEXT`); apply env overrides — `JACO_SERVER` (single addr or comma list), `JACO_TOKEN`, `JACO_CA_CERT`.
- [ ] Create `internal/cliclient/client.go` building `*grpc.ClientConn` with `credentials.NewClientTLSFromFile(CACertPath, "")`; attach unary + stream interceptors that set `authorization: Bearer <Token>` on every request.
- [ ] Implement rotation: a unary interceptor that, on `codes.Unavailable` / `codes.Internal` carrying `Error.code == "no_leader"` / TLS verify errors / dial timeouts, switches to the next `ServerAddrs` entry and retries. Round-robin across the list, max one full sweep per call.
- [ ] Streaming RPCs do NOT rotate mid-stream — on stream break, surface the error and exit non-zero (operator re-runs).
- [ ] Create `internal/cliclient/context_test.go`: write a `clusters.yaml` at 0644 → `Resolve` errors with "expected mode 0600, got 0644"; at 0600 succeeds; env overrides replace fields.
- [ ] Create `internal/cliclient/client_test.go`: mock 3 servers; first refuses connection, second returns `no_leader`, third accepts → call succeeds via rotation. Streaming call breaks mid-stream → no rotation, error surfaced.

## Acceptance criteria
- [ ] `go test ./internal/cliclient/... -race -count=1` exits 0.
- [ ] Test asserts file-mode reject error message includes the actual octal mode.
- [ ] Rotation test asserts the third server is used.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
