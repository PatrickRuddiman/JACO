Parent slice: [runtime](../slices/runtime.md)
Depends on: 04, 13

# Task 16 — docker-client-and-pull

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Docker client wrapper, volumes preflight, and exponential-backoff image pull with no upper attempt cap, capped at 1h per retry.

## Tasks
- [ ] Add `github.com/docker/docker/client` (moby) to `go.mod`.
- [ ] Create `internal/runtime/dockerx/client.go` with `New(socket string) (*Client, error)` defaulting to `unix:///var/run/docker.sock`; sets `client.WithAPIVersionNegotiation`.
- [ ] Define a `Docker` interface in `internal/runtime/dockerx/iface.go` covering the methods used elsewhere (`ContainerCreate, ContainerStart, ContainerStop, ContainerRemove, ContainerInspect, ContainerList, ContainerLogs, ImagePull, VolumeCreate, NetworkConnect`) so other packages can mock.
- [ ] Create `internal/runtime/volumes/volumes.go` with `EnsureNamedVolume(ctx, d Docker, name string) error` (idempotent VolumeCreate) and `ValidateBindMount(src string) error` (must exist + be readable, return `Error{code:"bind_mount_invalid", details}` otherwise).
- [ ] Create `internal/runtime/pull/pull.go` with `Pull(ctx, d Docker, image string, onState func(state string, attempt int, nextRetryAt time.Time)) error`. Backoff sequence `1s, 2s, 4s, ..., capped at 3600s`. No max attempts; runs until `ctx` cancelled. On `ctx.Done()` returns `ctx.Err()`. Reset by callers issuing a fresh context (Deploy.Apply does this — task 17 wires it).
- [ ] Use a clock abstraction (`type Clock interface { Now() time.Time; After(d time.Duration) <-chan time.Time }`) for deterministic tests.
- [ ] Create unit tests with a fake `Docker` and fake clock: pull succeeds after N transient failures; attempt N waits `min(2^(N-1), 3600)` seconds.

## Acceptance criteria
- [ ] `go test ./internal/runtime/dockerx/... ./internal/runtime/volumes/... ./internal/runtime/pull/... -race -count=1` exits 0.
- [ ] Test asserts attempt 13 sleeps exactly 3600s (cap reached at `2^12 = 4096 > 3600`).
- [ ] Test asserts cancellation via context returns `context.Canceled`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
