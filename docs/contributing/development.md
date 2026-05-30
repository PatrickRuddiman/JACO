---
sources:
  - Makefile
  - go.mod
  - .golangci.yml
  - .github/workflows/ci.yml
  - proto/jaco/v1/
  - internal/logging/
---

# Development

Day-to-day build, test, vet, lint, and proto workflow. All targets are
in the top-level [`Makefile`](../../Makefile); the linter is configured
by [`.golangci.yml`](../../.golangci.yml).

## Toolchain

- **Go** — pinned by `go.mod`. CI uses `setup-go@v5` with
  `go-version-file: go.mod`. Match locally with whatever pins the same
  version (`gvm`, `goenv`, `asdf`, or just install the matching tag).
- **buf** — used by `make proto`. Install per
  <https://buf.build/docs/installation>.
- **nfpm** — used by `make package`. Install with
  `go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.46.3`.
- **Docker** — required for runtime tests and the isolation rig.
- **nftables + wireguard-tools** — for the integration / isolation
  paths.

## Make targets

| target                 | runs                                                  |
|------------------------|-------------------------------------------------------|
| `make build`           | `go build -o jaco ./cmd/jaco`                         |
| `make test`            | `go test ./... -race -count=1`                        |
| `make ci-test`         | mirrors CI: `-race -coverprofile -skip <known flake>` |
| `make test-isolation`  | runs `scripts/test/isolation-rig.sh` (privileged)     |
| `make vet`             | `go vet ./...`                                        |
| `make lint`            | `vet` + `gofmt -l` check                              |
| `make proto`           | `buf generate` (regenerates `pkg/proto/jaco/v1/`)     |
| `make package`         | builds `.deb`/`.rpm`/`.apk` locally via nfpm          |
| `make release`         | cross-builds linux + darwin × amd64 + arm64 tarballs  |
| `make clean`           | removes `./jaco` and `dist/`                          |

`make ci-test` skips `TestExportImport_RoundTripPreservesBootstrapToken` —
a known snapshot-rename timestamp-collision flake tracked separately.

## Working with `internal/`

Subsystems are wired with explicit dependencies, no global state, no
`init()` magic:

- **Loggers** are passed in. Never reach for `slog.Default()` from a
  subsystem. The package's own logger comes via constructor (or a
  field); derive children with `logging.Subsystem(base, "name")`.
  See [Observability](../concepts/observability.md) and
  [`internal/logging/`](../../internal/logging).
- **Watches** are subscribed via `internal/controlplane/watch`. Buffered
  channels with drop-newest-on-overflow; on overflow you get a synthetic
  `Resync` event so subscribers re-fetch full state.
- **Raft writes** route through the leader. Non-leader handlers
  forward via `Internal.Submit`. Do not call `raft.Apply` directly from
  a follower path.
- **Scheduler-side code** must self-gate on `leader.IsLeader()`.
  Subsystems that run on every node (runtime, ingress, discovery)
  do not need the gate.

## Adding a CLI subcommand

The CLI follows a consistent shape; copy
[`cmd/jaco/apply.go`](../../cmd/jaco/apply.go) as a template:

1. `cmd <name>Cmd() *cobra.Command` building the cobra command with
   flags.
2. `RunE` reads flags, calls `dialOperator(...)` to get a connection +
   auth decorator, sets a context deadline appropriate for the call.
3. A `runX(ctx, client, ..., out io.Writer) error` body taking the
   proto client so unit tests can inject a fake.
4. Add the command to the root in an `init()` block.

Follow the same flag set every operator command uses: `--server`,
`--token`, `--ca-cert`, `--socket`. Use `defaultCACertPath()` and
`socketDefault()` for the defaults.

## Adding a gRPC handler

1. Edit `proto/jaco/v1/services.proto` (or `entities.proto` for new
   message types).
2. `make proto`. Commit the regenerated `pkg/proto/jaco/v1/`
   alongside the source.
3. Implement the handler under
   `internal/controlplane/grpc/<file>.go`.
4. If the call mutates state, the handler builds a `Command{}` proto
   and routes it through raft (`raft.Apply` on the leader,
   `Internal.Submit` forwarded from a follower).
5. Add an admission rule under `internal/controlplane/admission/` if
   the call needs anything other than the default token gate.
6. Write a unit test in the same package + an integration test under
   `internal/controlplane/grpc/` if the call has cross-vertical
   effects.

## Linting expectations

Correctness-only linters (`errcheck`, `govet`, `ineffassign`,
`staticcheck`, `unused`). Style linters are intentionally off:

- `gofmt` is enforced separately by `make lint`'s `gofmt -l` check.
  Run `gofmt -w .` before pushing.
- Naming, capitalization, and comment-style suggestions from
  `staticcheck` are disabled — no value vs. churn.
- `golangci-lint` v2 schema. Pin matches CI's `v2.12.2` (the first
  release built with go1.25, which the module pins).

Per-file or per-rule exemptions live in `.golangci.yml::issues.exclusions`.

## Branch hygiene

- One logical change per PR. Keep generated proto changes in their
  own commit (`make proto`).
- Run `make ci-test vet lint` locally before pushing.
- `make test-isolation` requires CAP_NET_ADMIN + CAP_NET_RAW + kernel
  WG + nftables + docker. CI runs it under a privileged runner;
  locally set `JACO_RIG_FORCE=1` once you've confirmed the host has
  what it needs.

## See also

- [Repository layout](repo-layout.md)
- [Testing](testing.md)
- [Release and packaging](release-and-packaging.md)
- [Observability](../concepts/observability.md)
