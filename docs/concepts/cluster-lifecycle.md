---
sources:
  - internal/controlplane/bootstrap/
  - internal/controlplane/raft/
  - internal/controlplane/grpc/cluster.go
  - internal/controlplane/grpc/membership.go
  - internal/scheduler/drain/
  - cmd/jaco/cluster.go
  - cmd/jaco/node.go
---

# Cluster lifecycle

How a cluster comes into existence, grows, elects leaders, and
shrinks. This page is the operator-facing summary.

## States

A `jacod` is in exactly one of three states at any time:

- **Uninitialized** — raft state directory is empty. Only
  `Cluster.{Init, Join, Status}` accept; every other RPC returns
  `cluster_uninitialized`. This is the state after a fresh install.
- **Initialized, raft member** — full RPC surface is open (token-gated
  on TCP, socket-trust on the local unix socket). The node is a
  voter in the raft group.
- **Initialized, isolation_unavailable** — initialized but the
  nftables ruleset failed to load (kernel without nftables, missing
  `nft` binary, missing `CAP_NET_ADMIN`). The node refuses to schedule
  replicas; other nodes see this and skip it for placement. See
  [Isolation](isolation.md).

## Bootstrap (first node)

```sh
sudo jaco cluster init
```

What the daemon does:

1. Generates a cluster id (UUID) and an Ed25519 cluster CA.
2. Generates a node TLS cert signed by the CA, persists it under
   `$JACO_DATA_DIR/node/`.
3. Initializes raft with `BootstrapCluster=true` and a single voter
   (itself).
4. Applies a seed `Command{ClusterInit}` carrying the cluster id, CA
   material, and the first operator token (identity `bootstrap`).
5. Flips the admission gate from `cluster_uninitialized` to fully
   open.
6. Prints the operator token once; the plaintext is never recoverable.

The cluster CA private key lives in the raft state, replicated to every
node. It never leaves the cluster boundary. Per-node server keys are
generated locally and never replicated.

## Join (subsequent nodes)

Two steps. First, on an initialized node, mint a single-use, 24-hour
token:

```sh
JACO_TOKEN=<op> jaco node issue-join-token --server <leader>:7000
```

The hashed token plus an expiry is written to raft as a
`JoinToken{}` entity. The plaintext is printed once.

Second, on the joining node:

```sh
sudo jaco node join --peer <leader>:7000 --token <hex>
```

What the joining daemon does:

1. Generates a CSR locally.
2. Dials `--peer` over TLS, presents the join token plus the CSR.
3. The leader validates the token (marks `consumed_at`), signs the
   CSR, returns the cluster CA + signed cert + raft peer set.
4. The joining daemon writes the cert + key, opens its raft node, and
   dials the existing peers.
5. The leader raft-applies `Command{NodeJoin{hostname, address}}`; the
   new node appears in `state.Nodes` and `jaco node list` on every
   member.
6. The joiner enters raft as a **nonvoter** (it replicates the log but
   doesn't count toward quorum). The leader's voter-set reconciler
   then promotes it to voter or leaves it as a nonvoter according to
   the [odd-count rule](#voter-set-policy) below.

The join token is single-use. A consumed token cannot be reused; a
fresh one must be issued.

## Voter-set policy

Joining nodes start as raft **nonvoters** and the leader-side
reconciler decides whether to promote them to voters. The policy is a
pure function of the current cluster member count:

| Members | Voters | Failures tolerated |
|---:|---:|---:|
| 1 | 1 | 0 |
| 2 | 1 | 0 |
| 3 | 3 | 1 |
| 4 | 3 | 1 |
| 5 | 5 | 2 |
| 6 | 5 | 2 |
| 7 | 7 | 3 |
| 8+ | 7 | 3 |

Two properties this guarantees:

- **Voter counts are odd.** Even voter counts buy nothing — a 4-voter
  cluster tolerates the same single failure as 3 voters but pays an
  extra ack on every commit. The reconciler skips the even rung.
- **Voter count is capped at 7.** A 7-voter cluster already tolerates
  3 simultaneous failures; more voters add commit latency without
  meaningful resilience improvement (the etcd / consul recommendation).

Each tick, the leader-side reconciler nudges the actual voter count
toward this target one suffrage change at a time — promoting the
lexicographically-first nonvoter or demoting the
lexicographically-last voter (excluding the leader, which never
demotes itself). Determinism across leaders means a failover doesn't
oscillate the voter set.

**Promotion is gated on catch-up.** A nonvoter must have been observed
in the raft configuration for at least the reconciler's `PromoteAfter`
window (3 s by default) before it becomes eligible. This defends the
1 → 2 bug-003 race: `AddNonvoter` commits the moment the
configuration-change log entry replicates, but the joiner's transport
may still be racing to catch up the rest of the log. The settle window
gives raft time to surface a failed peer before its vote can wedge
commits.

On graceful remove, the reverse holds: if removing the leaver would
drop the cluster below its post-remove target, the handler demotes
excess voters **before** issuing `RemoveServer`, so the cluster never
lands in a `voters > members − failure_budget` window.

`jaco cluster status` surfaces the per-node suffrage as `[STATUS,
VOTER]` or `[STATUS, NONVOTER]` so operators can verify the shape at a
glance.

## Leader election

Raft handles election. Practically:

- One voter at a time is leader; the others are followers. Writes
  go through the leader; reads can be served from any node's local
  watch cache.
- On a leader becoming unreachable, the remaining voters elect a new
  leader within the raft election timeout. The spec's bar is **a new
  leader within 10 s**. During the window, write RPCs return
  `no_leader, retrying`; once the new leader exists, retries succeed.
- A cluster with V voters tolerates `⌊(V−1)/2⌋` simultaneous **voter**
  losses without losing write availability. The voter target table in
  [Voter-set policy](#voter-set-policy) maps cluster size to V; e.g.
  a 5-member cluster has 5 voters and tolerates 2 voter losses, while
  a 4-member cluster has 3 voters and tolerates 1. Nonvoter losses
  don't count against the failure budget — they're spare capacity.

`jaco cluster status` reports the current leader; `jaco status` is
served from any node's local watch cache.

## Graceful remove

```sh
jaco node remove --server $LEADER <hostname>
```

The scheduler's drain machine:

1. Enumerates `ReplicaDesired` where `host = <hostname>`.
2. For each, computes a new host via the placement rules on the
   remaining eligible set and writes `ReplicaDesired{id, host:
   new_host}`. The old replica on the leaving host remains running
   and routable until its replacement passes health. **`placement:
   global`** replicas skip migration and are dropped instead —
   surviving nodes already host their own daemonset replica, and a
   migration would double-place it.
3. When all replacements report `running`, writes `ReplicaDesired{id,
   host: removed}`; runtime on the leaving node stops + removes
   containers.
4. Issues `Cluster.NodeRemove(<hostname>)`; FSM removes the `Node`
   entity. Discovery on every other node drops the WG peer entry.

`--force` skips drain enforcement when the node hosts pinned replicas
that cannot be relocated. Without `--force`, the request is rejected
with `node hosts pinned replicas: [...]`.

Per-replica drain timeout is 5 minutes. Exceeding it aborts the drain
with `pending: drain_timeout` visible in `jaco status`.

## Failure modes

- **Leader unreachable** — see "Leader election" above. Writes return
  `no_leader` until election settles; reads continue from local watch
  cache.
- **Network partition, minority side** — `quorum_lost`; writes
  rejected. Existing replicas on the minority side keep running;
  ingress continues serving from local healthy replicas. No new
  scheduling. The partition heals when network connectivity returns
  and the minority rejoins as followers.
- **Total cluster loss** — restore on a fresh host from `jaco backup`
  → `jaco restore` (see [Backups](../operations/backups.md) and
  [Recovery](../operations/recovery.md)). Restored state reflects
  every deployment committed before the snapshot.

## See also

- [`jaco cluster`](../cli/cluster.md)
- [`jaco node`](../cli/node.md)
- [Auth and tokens](auth-and-tokens.md)
- [Recovery](../operations/recovery.md)
