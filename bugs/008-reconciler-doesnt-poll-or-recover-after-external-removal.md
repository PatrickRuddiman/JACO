# BUG 008 — Reconciler doesn't recover from external container removal

## Symptom

If a container that the runtime reconciler created is deleted out of
band (`docker rm -f`, the daemon goes through a teardown attempt that
fails, etc.), the reconciler doesn't notice. `jaco status` keeps
reporting the last known ReplicaObserved state (FAILED in this case);
no new container gets created until something else mutates
ReplicaDesired.

## Severity

Medium. Doesn't block normal flow (operator wouldn't manually `docker
rm -f`), but it means the daemon can't self-heal from drift: an
operator who fixes a broken host by removing the stuck containers has
to also bump the deployment revision to re-trigger reconcile.

## Root cause

The runtime reconciler is purely event-driven on
`state.ReplicasDesired.Subscribe()`. It doesn't poll docker; it only
acts on raft watch events. If the docker state diverges from the
desired state without a corresponding raft event, the reconciler
never notices.

The scheduler's 30s safety tick re-runs reconcile but only at the
state→raft layer. State already says the replica is "desired here";
the runtime side never gets re-pinged.

## Fix

Two complementary options:

(a) Add a per-reconciler safety tick (30s, mirroring the scheduler's
    cadence) that calls `resync` so the runtime re-walks
    state.ReplicasDesired host=self and starts anything missing.

(b) Have the health.Watcher detect the "container disappeared"
    transition and submit a ReplicaObserved{state=PENDING, code=
    container_missing} update. The reconciler subscribes to
    ReplicasObserved through the existing watch and re-fires
    startReplica when an observation flips a desired replica to
    container_missing.

(a) is simpler and self-contained. Shipping (a) now.

## Status

**FIXING NOW.**
