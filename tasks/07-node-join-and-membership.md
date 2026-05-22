Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 06

# Task 07 — node-join-and-membership

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement single-use join tokens, CSR signing for joining nodes, raft `AddVoter` membership add, and the `jaco node {join,remove,list}` CLI subcommands.

## Tasks
- [x] Extend `pb.ClusterInit` with `self_hostname` + `self_address` so the FSM can populate `state.Nodes` for the bootstrap node (so `NodeList` reflects the cluster from the first node onward).
- [x] Wire `bootstrap.Run` to capture the raft transport address (`rnode.LocalAddr()`) and pass it via the new ClusterInit fields.
- [x] Add `admission.UnauthMethods` whitelist; register `/jaco.v1.Cluster/NodeJoin` (the join_token in the request body is the auth gate).
- [x] Extend `grpcsrv.Options` with `Raft *raftnode.Node` so handlers can call `AddVoter` / `RemoveServer`.
- [x] Fix `raftnode.Node.Shutdown` to close the underlying `*boltdb.BoltStore` (and any closable transport) so the same data dir can be re-opened immediately. Without this the bolt file lock leaked and re-opening the dir hung indefinitely.
- [x] Add `Cluster.IssueJoinToken` (operator-authenticated) in `internal/controlplane/grpc/membership.go`. Token: 32 random bytes hex-encoded; stored as `JoinToken{hashed_secret, issued_at, expires_at=now+24h, consumed_at=nil}` via raft Apply; returns the cleartext token + cluster CA + known peer addresses.
- [x] Add `Cluster.NodeJoin` (unauthenticated; gated by `join_token`). Validates token state (`join_token_invalid`, `join_token_consumed`, `join_token_expired`), signs the CSR via the CA in `state.Cluster`, calls `raft.AddVoter`, and raft-Applies a `Batch{JoinTokenConsume, NodeJoin}` so both updates land atomically. Returns the signed cert + CA + peer addrs.
- [x] Add `Cluster.NodeRemove` (operator-authenticated). Calls `raft.RemoveServer` and raft-Applies `Command{NodeRemove}`. Drain enforcement under `force=false` is a TODO until task 23 lands; for now NodeRemove always proceeds.
- [x] `Cluster.NodeList` (already in place from task 06).
- [x] Create `cmd/jaco/node.go` with `jaco node issue-join-token / join / remove / list`. The `join` subcommand generates the keypair, dials with `--ca-cert` pinned, calls `Cluster.NodeJoin`, and writes `${DATA}/node/{name}.{key,crt}` + `${DATA}/node/ca.crt` + a `join.json` carrying cluster_id + peer_addrs for `jaco serve` to consume (the "start raft as a follower" piece lands in task 17 alongside the daemon entry).
- [x] Create `internal/controlplane/grpc/node_join_test.go`: bootstrap A (preallocated port so the recorded raft address survives reopen), re-open A's raft post-bootstrap, start gRPC server, start B's raft on a second port (no bootstrap), drive the full IssueJoinToken→NodeJoin→NodeList flow, plus replay-the-same-join-token / unknown-token negative cases. Adds a NodeRemove test asserting eviction propagates to both raft and state, and a no-raft-wired NodeJoin error path.
- [ ] **Deferred to task 17**: `scripts/test/cluster-join.sh` — depends on `jaco serve` (daemon entry) to actually start raft as a follower on the joining node. The Go integration test exercises the same handshake end-to-end with in-process raft daemons.

## Acceptance criteria
- [x] `go test ./internal/controlplane/grpc/... -race -count=1 -run NodeJoin` exits 0.
- [x] Test asserts join token consumed twice → second call returns `Error.code == "join_token_consumed"`.
- [ ] `bash scripts/test/cluster-join.sh` exits 0 — deferred to task 17 per above.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
