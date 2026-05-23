# BUG 003 — NodeJoin AddVoter loses leadership when 1-node → 2-node

## Symptom

After `jaco cluster init` (single voter), `jaco node join` returns:

```
rpc error: code = Internal
  desc = node join rpc: rpc error: code = Internal
    desc = raft_apply_failed: leadership lost while committing log
```

`jaco cluster status` then reports:
```
Leader: (no leader elected)
```

…because the leader stepped down. State on disk shows raft_index
advanced past the AddVoter entry but the post-add Batch never landed.

## Severity

**Blocking.** Multi-node cluster bring-up impossible without working
around or fixing this.

## Root cause

`internal/daemon/grpc/membership.go::NodeJoin` calls
`raft.AddVoter(...)` synchronously before returning. Raft promotes the
new server to voting immediately; quorum size jumps from 1 to 2; the
leader needs the new voter to ack the AddVoter log entry, but the
joiner's raft hasn't started yet (operator-side Cluster.Join opens
raft only AFTER NodeJoin returns). Leader times out waiting for quorum
and steps down; the follow-up `applyCommand(batch)` then fails with
"leadership lost while committing log".

## Fix

Switch `AddVoter` → `AddNonvoter` in the NodeJoin handler. Non-voters
receive log replication but don't count toward quorum, so the 1-node
leader stays leader while the joiner spins up. A separate promotion
path (scheduler tick or NodeStatusUpdate-driven hook) flips
non-voters to voters once they're observed replicating.

For v0: ship the AddNonvoter change. The auto-promotion lands later;
operators get a working multi-node cluster where one of the nodes
isn't a quorum participant until manually promoted, which is the
correct behavior anyway during the brief window where the joiner is
catching up via snapshot.

## Status

**FIXING NOW** in the same iter that surfaced it.
