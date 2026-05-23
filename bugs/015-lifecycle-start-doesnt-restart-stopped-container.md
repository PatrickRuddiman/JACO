# BUG 015 — lifecycle.Start idempotent path doesn't restart stopped container

## Symptom

After Azure VM reboot, containers exist (`docker ps -a` shows
`jaco_hello-web-0 Exited (0) 18 minutes ago`) but `docker ps` is
empty. `jaco status` reports FAILED for every replica. Daemon's
reconciler safety tick fires but doesn't recover.

## Severity

**Blocking for reboot recovery.** Cluster doesn't self-heal after
host restart.

## Root cause

`internal/runtime/lifecycle/lifecycle.go::Start` has an idempotency
short-circuit:
```go
if existing != nil {
    if matchesRaftIndex(existing.Labels, spec.RaftIndex) {
        return existing.ID, nil  // no-op
    }
    if err := stopAndRemove(...); err != nil { ... }
}
```

When raft_index labels match the container is treated as "in sync"
and Start returns without re-checking the container's State.
A reboot can leave the container in `Exited (0)` while the labels
still match — the short-circuit incorrectly assumes "label match
== container running."

## Fix

In the idempotent branch, also inspect the container's running state.
When `state == "exited"` (or anything other than running/created with
healthy run), call `d.ContainerStart` to bring it back. Only return
the no-op early when the container is actually running.

## Status

**FIXING NOW.**
