# Cluster lifecycle

How a cluster comes into existence, grows, elects leaders, and
shrinks. The canonical decision-by-decision rationale lives in
[`slices/control-plane.md`](../planning/slices/control-plane.md) and
[`slices/daemon.md`](../planning/slices/daemon.md); this page is the
operator-facing summary.

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

The join token is single-use. A consumed token cannot be reused; a
fresh one must be issued.

## Leader election

Raft handles election. Practically:

- One voter at a time is leader; the others are followers. Writes
  go through the leader; reads can be served from any node's local
  watch cache.
- On a leader becoming unreachable, the remaining voters elect a new
  leader within the raft election timeout. The spec's bar is **a new
  leader within 10 s**. During the window, write RPCs return
  `no_leader, retrying`; once the new leader exists, retries succeed.
- A cluster of N nodes tolerates `⌊(N−1)/2⌋` simultaneous failures
  without losing write availability. A 3-node cluster survives one
  loss; a 5-node cluster survives two.

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
   and routable until its replacement passes health.
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
- [`slices/control-plane.md`](../planning/slices/control-plane.md)
