Parent slice: [runtime](../slices/runtime.md)
Depends on: 04, 13

# Task 16 — docker-client-and-pull

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Docker client wrapper, volumes preflight, and exponential-backoff image pull with no upper attempt cap, capped at 1h per retry.

## Tasks
- [x] Add `github.com/docker/docker` v28.5.2 to `go.mod` (the module path is still `github.com/docker/docker` even though the upstream now lives at `github.com/moby/moby`).
- [x] Create `internal/runtime/dockerx/client.go` with `New(socket string) (*Client, error)` defaulting to `DefaultSocket = "unix:///var/run/docker.sock"`; uses `client.WithAPIVersionNegotiation`. Client embeds the real *client.Client so it satisfies the narrow Docker interface implicitly.
- [x] Define `Docker` interface in `internal/runtime/dockerx/iface.go` covering ContainerCreate / Start / Stop / Remove / Inspect / List / Logs, ImagePull, VolumeCreate, NetworkConnect with the exact moby docker-client signatures. Consumers take this interface so tests can mock via partial fakes.
- [x] Create `internal/runtime/volumes/volumes.go` with `EnsureNamedVolume(ctx, d, name)` (idempotent VolumeCreate) and `ValidateBindMount(src)` returning typed `*volumes.Error{Code:"bind_mount_invalid", Path, Message}` when src is missing / unreadable / empty. `IsBindMountInvalid(err)` is the helper for callers gating on it.
- [x] Create `internal/runtime/pull/pull.go` with `Pull(ctx, d, ref, clock, onState)` retrying ImagePull with `BackoffDuration(attempt)` (1s, 2s, 4s, … capped at `BackoffCap = 1h`). No max attempts; ctx-cancel resets. StateFn receives `(state, attempt, nextRetryAt, lastErr)` transitions — pulling / failed / done — for task 18 to wire into ReplicaObserved writes.
- [x] Clock interface abstracts time.Now + time.After. SystemClock() returns the production implementation; tests use a fakeClock that auto-advances on After() and records every requested delay for assertion.
- [x] Twelve tests pass with -race: VolumeCreate-wires-the-name + empty-name-rejected + docker-error-propagated, ValidateBindMount existing-dir + existing-file, missing-path returns typed error, empty-source rejected. BackoffDuration sequence (attempt 1=1s, 12=2048s, 13=3600s, 100=3600s). Pull succeeds after 3 transient failures with the 1s/2s/4s sequence. Cap-at-3600s-on-attempt-13 (the AC). Context cancellation returns context.Canceled (the AC). Empty-ref rejected. StateFn transitions pulling→failed→pulling→failed→pulling→done.

## Acceptance criteria
- [x] `go test ./internal/runtime/dockerx/... ./internal/runtime/volumes/... ./internal/runtime/pull/... -race -count=1` exits 0.
- [x] Test asserts attempt 13 sleeps exactly 3600s (`TestPull_BackoffSequenceCapsAt3600AtAttempt13` + `TestBackoffDuration_Sequence`).
- [x] Test asserts cancellation via context returns `context.Canceled` (`TestPull_CancellationReturnsContextErr`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
