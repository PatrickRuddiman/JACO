Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 05

# Task 06 — grpc-server-and-admission

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Stand up the gRPC server on `:7000` with TLS terminated by the node's CA-signed cert, attach the bearer-token admission interceptor, and forward leader-required writes to the current raft leader.

## Tasks
- [x] Create `internal/controlplane/grpc/server.go` (package `grpcsrv`, avoids clash with `google.golang.org/grpc`) building `*grpc.Server` with TLS credentials parsed from `Options.NodeCert`/`NodeKey` (PEM bytes) plus the admission interceptors wired in. `Options.BindAddr` is required; `Addr()` exposes the bound address (handy when port=0). `Serve` blocks; `Stop` graceful-stops.
- [x] Create `internal/controlplane/admission/admission.go` exposing `UnaryInterceptor(s *state.State)` and `StreamInterceptor(s *state.State)`. Each extracts `authorization` metadata, strips `Bearer `, SHA-256-hashes the token, calls `state.LookupTokenByHash`. On miss: `codes.Unauthenticated` whose status message is `token_invalid` with a typed `pb.Error{code:token_invalid}` attached via `WithDetails`. On revoked (`Token.RevokedAt != nil`): same envelope with code `token_revoked`. On success: attach the identity to `context.Context` under a private typed key (`IdentityFromContext` reader).
- [x] Add `state.LookupTokenByHash(*Store[*pb.Token], hash []byte)` in `internal/controlplane/state/tokens.go` — linear-scan helper since the generic `Store[T]` indexes only by primary key.
- [x] Create `internal/controlplane/grpc/forward.go` exposing `LeaderForwarder.EnsureLeader(ctx) (pb.InternalClient, isLocal bool, err error)`. If local node is leader: `isLocal=true`, no client. Otherwise dial the leader (`grpc.NewClient` over TLS verified against the cluster CA) and return an `Internal` client; returns `Error{code:"no_leader"}` if the leader is unknown.
- [x] Implementing `Internal.Submit` (raft.Apply passthrough) is deferred to task 07 alongside the rest of the Internal service. For task 06 the forward helper is in place and exercised via the local-leader path (no remote dial in this test).
- [x] Add `internal/controlplane/grpc/cluster.go` registering the Cluster service with `Status` (returns Nodes list + leader address) and `NodeList`; the rest inherits from `pb.UnimplementedClusterServer` until their task lands.
- [x] Create `internal/controlplane/grpc/server_test.go`: generate CA + node cert (127.0.0.1 SAN), build state with a known operator token, start the gRPC server on `127.0.0.1:0`, dial with the CA-trusting client. Valid token → `Status` succeeds. Bad token → `codes.Unauthenticated` with message containing `token_invalid` AND a typed `pb.Error{code:token_invalid}` in status details. No-token path: same Unauthenticated outcome. Plus seven admission unit tests covering missing metadata, missing header, non-Bearer scheme, unknown token, revoked token, and typed-Error-detail wiring.

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/... ./internal/controlplane/admission/... -race -count=1` exits 0.
- [x] `git grep -nE '"token_invalid"|"token_revoked"' internal/controlplane/admission/admission.go` returns both matches.
- [x] Test asserts an unauthenticated call surfaces `codes.Unauthenticated` with body containing `token_invalid`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
