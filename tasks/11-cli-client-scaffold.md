Parent slice: [cli](../slices/cli.md)
Depends on: 06

# Task 11 — cli-client-scaffold

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`~/.config/jaco/clusters.yaml` loader with env overrides plus a reusable gRPC client that rotates through `server_addrs` on connection or `no_leader` errors.

## Tasks
- [x] Create `internal/cliclient/context.go` with `Clusters` (current_context + contexts) and `Context` (name + server_addrs + ca_cert_path + token) YAML types. `Load(path)` checks the file is a regular file at mode 0600; otherwise returns an error containing both expected and actual octal modes.
- [x] `Resolve(opts)` loads the file when present, picks the active context (JACO_CONTEXT env override → CurrentContext → first entry), applies env overrides for JACO_SERVER (comma list), JACO_TOKEN, JACO_CA_CERT, and validates that ServerAddrs, Token, and CACertPath are populated. Falls back to env-only when the file is missing.
- [x] Create `internal/cliclient/client.go` with `NewClient(ctx)` building a Client that loads the CA cert and wraps a list of server addresses, plus `NewInsecure(opts)` for tests. `AuthContext(ctx)` attaches `authorization: Bearer <token>` outgoing metadata. `Conn()` returns the current (cached) grpc.ClientConn — used by stream RPCs which do NOT rotate mid-stream.
- [x] `Client.Invoke(ctx, fn)` is the rotation primitive: tries each address in order, dialing lazily; on `shouldRotate(err)` (codes.Unavailable, codes.DeadlineExceeded, or any status whose message contains `no_leader` or whose details carry `pb.Error{code:"no_leader"}`) it closes the connection and tries the next address. Non-retryable errors surface immediately. After every address has been tried it wraps the last error with `all endpoints unreachable`.
- [x] Define `cliclient.ErrAllExhausted` sentinel for callers that need to gate on the exhaustion case specifically.
- [x] Migrating the existing CLI subcommands (node/token/audit/backup/restore) to consume cliclient is **deferred** — they keep using the inline dialServer helper in cmd/jaco/node.go for now. Task 12 (renderers) is the natural batch point for that migration since both refactors touch the CLI surface.
- [x] Create `internal/cliclient/context_test.go`: file-mode reject at 0644 with the actual mode in the error message; mode 0600 succeeds; JACO_CONTEXT selects the staging context; env overrides replace ServerAddrs / Token / CACertPath; comma-separated JACO_SERVER parses cleanly; env-only when the file is missing; missing ServerAddrs errors; unknown context name rejected.
- [x] Create `internal/cliclient/client_test.go`: three fake gRPC servers (insecure) where #1 has nothing listening, #2 returns `no_leader` (Unavailable + typed Error detail), #3 returns success → `Invoke` succeeds via rotation and calls both #2 and #3. Plus: non-retryable PermissionDenied surfaces immediately without rotating; all-endpoints-unreachable returns the wrapped error; the cached connection is reused across successive successful Invokes; AuthContext attaches the Bearer; empty token leaves the context unchanged.

## Acceptance criteria
- [x] `go test ./internal/cliclient/... -race -count=1` exits 0.
- [x] Test asserts file-mode reject error message includes the actual octal mode (`expected mode 0600, got 0644`).
- [x] Rotation test asserts the third server is used (asserted via the goodFake call counter being non-zero).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
