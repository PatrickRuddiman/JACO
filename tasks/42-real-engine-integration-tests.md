Parent slice: [packaging](../slices/packaging.md)
Depends on: 38

# Task 42 — real-engine-integration-tests

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Ship four build-tagged integration tests against real engines so the slice-specific unit tests' assumptions are validated end-to-end. Each test lives behind its own build tag so the default `go test ./...` skips them; CI runs them on a privileged Linux runner.

## Tasks
- [ ] `internal/runtime/lifecycle/lifecycle_integration_test.go`: file-level `//go:build docker` build tag. Skips when `JACO_INTEGRATION_DOCKER` isn't set or `dockerx.New("")` fails. Drives `lifecycle.Start` against the real docker daemon with `nginx:alpine`, asserts the container reaches RUNNING, then `Stop` + `Remove`, asserts the container is gone.
- [ ] `internal/discovery/firewall/firewall_integration_test.go`: `//go:build nftables`. Skips when `JACO_INTEGRATION_NFTABLES` isn't set or `firewall.IsAvailable()` errors. Calls `Render` against a single-subnet `RuleInput`, `Apply` it via `DefaultApplier`, runs `SelfTest` against the live ruleset, then tears down with `nft delete table inet jaco`.
- [ ] `internal/discovery/wgmesh/wgmesh_integration_test.go`: `//go:build wireguard`. Skips when `JACO_INTEGRATION_WG` isn't set or `wgmesh.IsKernelAvailable()` errors. Creates a dummy `jaco-test0` interface via `ip link`, runs `Syncer.tick` against a fake `state.State` with two peers, asserts `wgctrl.Client.Device("jaco-test0").Peers` matches the expected pubkeys, tears down.
- [ ] `internal/ingress/acme_integration_test.go`: `//go:build acme`. Skips when `JACO_INTEGRATION_PEBBLE` (URL to a running Pebble instance) isn't set. Issues a cert for `test.jaco.local` against Pebble, asserts the cert lands in `state.CertBlobs` via the storage layer (depends on task 40), asserts an `AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED` event was written.
- [ ] `scripts/test/integration.sh`: new shell harness that exports each `JACO_INTEGRATION_*` env var, runs `go test -tags <tag> ./...` for each tag in sequence. Idempotent: rerunning after a passing run is a no-op. Returns non-zero on any failure.
- [ ] `.github/workflows/integration.yml`: GitHub Actions workflow gated on a `privileged` label. Installs `docker.io`, `nftables`, `wireguard-tools`, `pebble`; runs `scripts/test/integration.sh`. Documented in the workflow comment that the runner needs `--privileged`.

## Acceptance criteria
- [ ] `go test ./... -race -count=1` exits 0 across the whole tree (build-tagged tests skip cleanly when env vars are unset).
- [ ] `go vet -tags='docker nftables wireguard acme' ./...` exits 0 — the build-tagged code compiles.
- [ ] `git grep -nE '//go:build docker' internal/runtime/lifecycle/` matches.
- [ ] `git grep -nE '//go:build nftables' internal/discovery/firewall/` matches.
- [ ] `git grep -nE '//go:build wireguard' internal/discovery/wgmesh/` matches.
- [ ] `git grep -nE '//go:build acme' internal/ingress/` matches.
- [ ] `test -f scripts/test/integration.sh && test -x scripts/test/integration.sh`.
- [ ] `test -f .github/workflows/integration.yml`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
