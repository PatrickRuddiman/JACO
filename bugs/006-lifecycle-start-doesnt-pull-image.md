# BUG 006 — lifecycle.Start doesn't pull image before ContainerCreate

## Symptom

After `jaco apply hello.yaml` against a 3-node cluster where the
referenced image isn't already cached locally, the runtime reconciler
errors:

```
start replica hello-web-0: lifecycle.Start: ContainerCreate:
  Error response from daemon: No such image: nginx:1.27-alpine
```

No container spawns; the reconciler retries on the next watch tick
and hits the same error.

## Severity

**Blocking** for any image not pre-cached on the host — which on a
fresh VM is every image.

## Root cause

`internal/runtime/lifecycle/lifecycle.go::Start` calls
`d.ContainerCreate` directly. It doesn't call `runtime/pull.Pull`
(which exists, with backoff + state callbacks, but never gets wired
in by Start).

## Fix

Have `Start` call `pull.Pull(ctx, d, spec.Image, ...)` before
`d.ContainerCreate` when `findContainerByReplicaID` returns nil
(i.e. when we're about to create a new container, not when we're
on the idempotent path because the replica already exists).

A nil onState callback is fine for now — the pull progress doesn't
need to surface in audit yet; the bool-state output of Pull bubbles
the error up to lifecycle.Start which returns it to the reconciler
which logs it.

## Status

**FIXING NOW.**
