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

The drain step machine raft-Applies `ReplicaDesiredUpsert{host: <new>}`
for each migrated replica and polls observed-RUNNING-on-new-host
before calling `raft.RemoveServer`, so the upsert *does* replicate to
the leaving node before raft cuts the cord. The reactive watch path
on the leaving node (reconciler.go:166-172) handles `KindUpdated` with
`Before.host=self, After.host=other` and calls `stopReplica`.

The deeper issue: the reconciler's contract is "converge local docker
state to state.ReplicasDesired host=self" — bidirectional. The reactive
watch path handled the host-diff case, but the 30s safety tick only
called `resync` which is **start-only**. So if the watch event was
missed or `stopReplica` errored, the safety tick had no mechanism to
notice the orphaned container and the leaving node ran the migrated
replica forever.

## Fix

The 30s safety tick now also runs `orphanSweep` (the same logic the
boot path called `bootSweep` for, just promoted to every tick).
`orphanSweep` lists every container labeled with our `cluster_id`
and stop+removes any whose `replica_id` isn't in
`state.ReplicasDesired` filtered to `host=self`. After a drain, the
leaving node's `expected` set no longer contains the migrated
replica → its container gets reaped within at most one tick (30s),
independent of whether the watch event was delivered or processed.

`bootSweep` renamed to `orphanSweep` since it's no longer
boot-specific.

Test: `TestReconciler_OrphanSweepStopsContainerWhenDesiredMovedHosts`
in `internal/runtime/reconciler/reconciler_test.go` pre-seeds a
container matching a replica that desired-state doesn't claim, calls
OrphanSweep, asserts it's gone.

## Status

**FIXED + LIVE-VERIFIED.**

Live verification 2026-05-23 on the 3-VM Azure cluster (bug16-test):
1. Fresh 3-node cluster up; `hello` deployed with 3 replicas, one per host
   (hello-web-0 on jaco-1, hello-web-1 on jaco-2, hello-web-2 on jaco-3).
2. `jaco node remove jaco-3` (graceful) returned in 1s.
3. Polled `docker ps --filter label=jaco.cluster_id` on jaco-3 at 5s
   intervals: at **t+2s** post-drain, jaco-3 had zero containers — the
   reactive watch path actually did fire correctly this run, and orphan
   sweep is the belt-and-suspenders that catches any missed event.
4. Final state: 2-node cluster (jaco-1 + jaco-2), 3 replicas RUNNING,
   jaco-3 clean.
