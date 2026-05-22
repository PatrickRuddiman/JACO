Parent slice: [control-plane](../slices/control-plane.md)
Depends on: 06

# Task 07 — node-join-and-membership

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Implement single-use join tokens, CSR signing for joining nodes, raft `AddVoter` membership add, and the `jaco node {join,remove,list}` CLI subcommands.

## Tasks
- [ ] Add `Cluster.IssueJoinToken(req) returns (JoinTokenResponse{token, ca_cert, leader_addrs})` handler in `internal/controlplane/grpc/cluster.go`. Token: 32 random bytes hex-encoded; store under `JoinToken{hashed_secret, issued_at, expires_at=now+24h, consumed_at=nil}` via raft Apply.
- [ ] Add `Cluster.NodeJoin(req{name, join_token, csr_pem, advertise_addr}) returns (NodeJoinResponse{cluster_id, signed_cert, ca_cert, peer_addrs})`. Validation: lookup hash; reject if expired or consumed; mark `consumed_at = now` via raft Apply. Sign the CSR using the cluster CA from raft state. raft.AddVoter for the new node. raft-Apply `Command{NodeJoin}{name, address, server_cert_fingerprint}`.
- [ ] Add `Cluster.NodeRemove(req{hostname, force}) returns (NodeRemoveResponse{})`: raft.RemoveServer + raft-Apply `Command{NodeRemove}{hostname}`. If `force=false`, requires the scheduler drain (task 23) to have completed first.
- [ ] Add `Cluster.NodeList(req) returns (NodeListResponse{nodes})` reading from local `state.Nodes`.
- [ ] Create `cmd/jaco/node.go` registering `jaco node join --address <host:port> --join-token <secret> --name <hostname>`, `jaco node remove <hostname> [--force]`, `jaco node list`. Join flow: generate keypair, build CSR, call `Cluster.NodeJoin`, write `${DATA}/node/<name>.{key,crt}` and ca cert, start raft as a follower.
- [ ] Create `internal/controlplane/grpc/node_join_test.go`: bootstrap node A; issue join token via gRPC; spin up node B against a second `t.TempDir()`; call `NodeJoin`; assert `NodeList` returns 2 nodes within 5s.
- [ ] Create `scripts/test/cluster-join.sh` E2E: bootstrap on host 1, issue join token, run `jaco node join` on hosts 2 and 3, assert `jaco node list` returns 3 rows.

## Acceptance criteria
- [ ] `go test ./internal/controlplane/grpc/... -race -count=1 -run NodeJoin` exits 0.
- [ ] `bash scripts/test/cluster-join.sh` exits 0.
- [ ] Test asserts join token consumed twice → second call returns `Error.code == "join_token_consumed"`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
