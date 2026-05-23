# BUG 016 — After NodeRemove(force=false), removed node keeps running its containers

## Symptom

After `jaco node remove jaco-3` (graceful drain succeeds), the
cluster shows 2 nodes and 3 replicas distributed across jaco-1 +
jaco-2 — but `docker ps` on jaco-3 still shows `jaco_hello-web-2 Up`.

## Severity

Medium. Cluster integrity isn't compromised (the removed node is no
longer in the raft membership) but the orphaned containers consume
resources on jaco-3 indefinitely until the operator manually removes
them or shuts the node down.

## Root cause

The drain step machine raft-Applies ReplicaDesiredUpsert{host: <new>}
records that migrate each replica off the leaving host BEFORE calling
`raft.RemoveServer`. The intent is that the runtime reconciler on the
leaving host sees the KindUpdated watch event (Before.host=jaco-3,
After.host=jaco-2) and calls stopReplica.

But after `raft.RemoveServer`, the leaving node's raft transport gets
torn down. The reconciler on the leaving node:
  (a) may have already seen the upsert and called stopReplica
      successfully — but my live test shows the container still up;
      either the watch fired before reconciler subscribed or stopReplica
      hit an error.
  (b) may not have seen the upsert if raft replication paused mid-batch.

## Fix (to design)

Two possible directions:

1. Reconciler explicitly subscribes to "node removed (self)" watch and,
   on hearing its own NodeRemove, stops every local replica it has.
2. drain.Plan emits a dedicated `ReplicaCommand{op: stop}` for every
   migrated replica targeting the leaving host, separate from the
   ReplicaDesiredUpsert that places it elsewhere. The leaving host's
   reconciler picks up the stop before raft tears down.

Option 2 is more explicit and easier to reason about. Lands in a
follow-up.

## Status

Open. Non-blocking for v0 cluster bring-up; logged for follow-up.
