Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 05

# Task 06 — grpc-server-and-admission

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Stand up the gRPC server on `:7000` with TLS terminated by the node's CA-signed cert, attach the bearer-token admission interceptor, and forward leader-required writes to the current raft leader.

## Tasks
- [ ] Create `internal/controlplane/grpc/server.go` constructing a `*grpc.Server` with `credentials.NewTLS(...)` loaded from `${DATA}/node/<name>.{crt,key}`. Listens on `--bind` (default `0.0.0.0:7000`).
- [ ] Create `internal/controlplane/admission/admission.go` exposing `UnaryInterceptor(store *state.Store)` and `StreamInterceptor(store *state.Store)`. Each: extract `authorization` metadata, strip `Bearer ` prefix, SHA-256-hash, `store.Tokens.GetByHash(...)`. On miss: return `status.Error(codes.Unauthenticated, marshalErr("token_invalid"))`. On revoked: `token_revoked`. On success: attach `identity` to `context.Context` via a typed key.
- [ ] Create `internal/controlplane/grpc/forward.go` exposing `EnsureLeader(ctx, raftNode *raft.Node) (leaderClient pb.InternalClient, isLocal bool, err error)`. If local node is not leader, dial the leader's TCP address (parse from raft leader address; gRPC port is the same `:7000`); if leader unknown, return `Error{code:"no_leader"}`.
- [ ] Add a `Submit` RPC to `Internal` service (was declared in 01) implemented as a passthrough on the leader: unmarshal payload, call `raft.Apply` directly.
- [ ] Create `internal/controlplane/grpc/server_test.go`: bootstrap via task 05 helpers; start the gRPC server; dial with the operator token → a no-op echo RPC succeeds; dial with garbage token → returns `Error.code == "token_invalid"`.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/grpc/... ./internal/controlplane/admission/... -race -count=1` exits 0.
- [ ] `git grep -nE '"token_invalid"|"token_revoked"' internal/controlplane/admission/admission.go` returns both matches.
- [ ] Test asserts an unauthenticated call surfaces `codes.Unauthenticated` with body containing `token_invalid`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
