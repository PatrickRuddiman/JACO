---
sources:
  - Makefile
  - .github/workflows/ci.yml
  - .github/workflows/integration.yml
  - .github/workflows/isolation-rig.yml
  - scripts/test/
  - tests/
---

# Testing

Three test surfaces, in increasing operational cost:

1. **Unit tests** — `go test ./...`; no engines required.
2. **Integration tests** — build-tag-gated, exercise real docker /
   nftables / wireguard / ACME engines.
3. **End-to-end rigs** — privileged shell scripts under
   [`scripts/test/`](../../scripts/test) that stand up multi-node
   clusters and assert observable cluster behavior.

Plus the comparative samples bench under
[`tests/samples/`](../../tests/samples) — not a CI gate, but the
reference workload for cross-orchestrator benchmarking.

## Unit tests

```sh
make test       # go test ./... -race -count=1
make ci-test    # mirrors CI: adds coverage + the known-flake skip
```

`make ci-test` skips `TestExportImport_RoundTripPreservesBootstrapToken`
— a snapshot-rename timestamp-collision flake tracked separately. The
same skip is hard-coded into
[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) so local
and CI signals match.

Conventions:

- One package per test file. Tests live next to the code they cover.
- Subsystem constructors take loggers + clients explicitly; tests
  inject fakes. Never reach for `slog.Default()` — a lint check in
  `internal/logging/forbid_default_test.go` catches it.
- Use the proto clients from `pkg/proto/jaco/v1/` even in tests;
  hand-rolled fakes implement just enough of the interface for the
  call under test.

## Integration tests

Build tags: `docker`, `nftables`, `wireguard`, `acme`. Each tag's
suite self-skips when the matching `JACO_INTEGRATION_*` env var is
unset, so a developer with only docker can still run the docker
suites without setting up the rest.

Driver: [`scripts/test/integration.sh`](../../scripts/test/integration.sh).
The packages it sweeps:

```
-tags docker:    ./internal/runtime/lifecycle/...
                 ./internal/runtime/logs/...
                 ./internal/runtime/health/...
-tags nftables:  ./internal/discovery/firewall/...
-tags wireguard: ./internal/discovery/wgmesh/...
-tags acme:      ./internal/ingress/...
```

Local run (needs root or matching capabilities):

```sh
JACO_INTEGRATION_DOCKER=1 \
JACO_INTEGRATION_NFTABLES=1 \
JACO_INTEGRATION_WG=1 \
JACO_INTEGRATION_PEBBLE=https://localhost:14000/dir \
sudo -E bash scripts/test/integration.sh
```

CI runs the full sweep in
[`.github/workflows/integration.yml`](../../.github/workflows/integration.yml),
gated on the `privileged` label so untrusted PRs don't consume the
privileged runner automatically. The same workflow runs the install
smoke test (`scripts/test/install.sh`), the isolation rig, and the
shell-based E2Es.

## Isolation rig

The isolation rig (`scripts/test/isolation-rig.sh`, runnable via
`make test-isolation`) is the canonical end-to-end test for the
spec's cross-deployment + cross-network isolation promises. It stands
up a 3-node cluster, applies two deployments each with two networks,
and asserts:

- **Positive** — same-(deployment, network) TCP and UDP succeed across
  nodes; DNS resolution succeeds in-network.
- **Negative** — cross-deployment TCP/UDP fails by IP; cross-deployment
  DNS returns NXDOMAIN; cross-network within deployment same.
- **Drift recovery** — flush `inet jaco` out-of-band; within 30 s the
  reconcile loop restores the ruleset and emits an
  `isolation_ruleset_reconciled` audit event.
- **Startup failure** — boot a daemon with `nft` unavailable; assert
  it never reaches ready and other nodes report
  `isolation_unavailable` for it.

Requires CAP_NET_ADMIN + CAP_NET_RAW + kernel WG + nftables + docker.
CI runs it under a privileged runner; locally, set `JACO_RIG_FORCE=1`
to confirm the host has what it needs.

## Other E2E rigs

Under [`scripts/test/`](../../scripts/test):

- `apply-deploy.sh` — applies a manifest pair, asserts convergence.
- `cluster-join.sh` — bootstraps + joins a 3-node cluster.
- `drain-node.sh` — exercises the graceful drain path.
- `ingress-acme.sh` — drives ACME issuance against Pebble.
- `install.sh` — runs the .deb/.rpm install + uninstall +
  idempotency tests.
- `isolation-drift.sh` — focused drift-recovery test (subset of the
  rig).
- `logs-fanout.sh` — verifies cross-node log streaming.
- `scheduler-spread.sh` — asserts placement distribution.
- `self-upgrade.sh` — exercises the verify + atomic-swap path.
- `status-watch.sh` — confirms `jaco status -w` re-renders on events.

Each self-skips unless its `JACO_*_FORCE=1` env is set, so the
integration workflow can sweep them all in sequence.

## Samples bench

The [`tests/samples/`](../../tests/samples) tree is a reproducible,
bias-controlled comparison of JACO against Kubernetes (kubeadm),
k3s, and Docker Swarm. One workload, identical resource limits,
graded by the same rubric. Not a CI gate; intended for periodic
benchmarking on the Azure substrate provisioned by
[`tests/testbed/`](../../tests/testbed).

Today only the JACO path is implemented end-to-end; the other three
are stubs waiting for their bootstrap + manifests + bench adapter.

## Behavior-pinning fixtures

Standalone fixture trees that pin a single invariant for live
verification on a real cluster — not CI-gated, intended for the
manual smoke run the relevant PR documents:

- [`tests/samples/jaco/smoke-volumes/`](../../tests/samples/jaco/smoke-volumes/README.md)
  — two co-located deployments that prove JACO scopes named compose
  volumes per deployment (`jaco_<deployment>_<key>`), plus an opt-out
  probe for the `volumes.<key>.name:` escape hatch. Companion unit
  test `internal/runtime/compose/smoke_fixtures_test.go` pins the
  fixture against `ToContainerSpec` so a refactor surfaces locally
  before the live smoke. Cross-linked from
  [`tests/isolation/README.md`](../../tests/isolation/README.md);
  promotion into the privileged 3-node isolation rig is the
  follow-up.
- [`internal/controlplane/raft/membership/integration_test.go`](../../internal/controlplane/raft/membership/integration_test.go)
  — exercises the voter-set reconciler across a `1→2→3→4→5→4→3→2→1`
  membership sequence against real raft nodes, asserting voter
  counts match the [voter-set policy](../concepts/cluster-lifecycle.md#voter-set-policy)
  at every step. Runs as a normal `go test`; no privileged surface.

## Test policy

- **No mocking the FSM.** Cross-vertical integration tests run a real
  raft node in `BootstrapCluster=true` mode against an in-memory
  bolt store. Mocking the FSM ships bugs.
- **No suppressing assertions to make tests pass.** A failing test is
  data — investigate before deciding it's flaky.
- **Behavior > plumbing.** Tests that assert a specific log line or
  config-default value churn every time someone reformats. Tests
  that assert "applying X causes Y to converge" survive refactors.

## See also

- [Development](development.md)
- [Repository layout](repo-layout.md)
- [Isolation](../concepts/isolation.md)
- [Observability](../concepts/observability.md)
