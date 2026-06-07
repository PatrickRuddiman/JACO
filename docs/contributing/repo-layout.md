---
sources:
  - cmd/
  - internal/
  - pkg/
  - proto/
  - scripts/test/
  - tests/
---

# Repository layout

What lives where. Use this as the map when you're about to grep.

```
.
├── cmd/                 # binary entry points
│   ├── jaco/            # operator + developer CLI
│   └── jacod/           # long-running daemon
├── internal/            # private packages; no external API surface
│   ├── cliclient/       # gRPC client builder for the CLI
│   ├── controlplane/    # raft FSM, admission, watch, gRPC handlers
│   │   ├── admission/   # bearer-token + unix-socket auth gate
│   │   ├── backup/      # snapshot export / import
│   │   ├── bootstrap/   # `Cluster.Init` daemon-side library
│   │   ├── grpc/        # server-side handlers for Cluster/Deploy/Audit/Token/Watch
│   │   ├── raft/        # hashicorp/raft wiring; membership/ runs leader-only voter-set reconciler (see concepts/cluster-lifecycle.md)
│   │   ├── state/       # typed entity stores
│   │   └── watch/       # per-entity pub/sub broker
│   ├── daemon/          # jacod-only: config + netdetect + admission gate
│   │   ├── admission/   # init-gate interceptor
│   │   ├── config/      # jacod.yaml schema + loader
│   │   ├── grpc/        # daemon-side server wiring
│   │   └── netdetect/   # private-LAN-first interface auto-detection
│   ├── discovery/       # bridges, IPAM, WG mesh, nftables, DNS
│   │   ├── bridge/      # docker bridge management
│   │   ├── dns/         # per-bridge DNS responder
│   │   ├── firewall/    # nftables ruleset render + reconcile + self-test
│   │   ├── ipam/        # /24 allocator
│   │   ├── runtime_attach/  # helper for runtime's container-create path
│   │   └── wgmesh/      # wireguard interface management
│   ├── ingress/         # embedded Caddy + custom certmagic.Storage
│   │   ├── cachepoke/   # go:linkname into caddytls.certCache for promote eviction (#163)
│   │   ├── challenge/   # HTTP-01 token coordination via raft
│   │   ├── config/      # state → Caddy JSON
│   │   ├── rebuild/     # debounced rebuild loop
│   │   ├── stagefirst/  # LE staging dry-run controller (issue #41) + pending-prod window (#154)
│   │   └── storage/     # certmagic.Storage backed by raft
│   ├── logging/         # log/slog convention; journal + JSON + text handlers
│   ├── packaging/       # tarball signature/checksum verify for self-upgrade
│   ├── runtime/         # docker engine driver
│   │   ├── cgroupv2/    # per-node CPU + memory pressure collector (Linux)
│   │   ├── compose/     # compose parser + closed-field validator + per-service spec_hash for drift detection (#148)
│   │   ├── dockerx/     # docker client glue
│   │   ├── health/      # per-replica healthcheck poller
│   │   ├── lifecycle/   # create / start / stop / remove
│   │   ├── logs/        # per-replica log tail
│   │   ├── pull/        # image pull with backoff
│   │   ├── reconciler/  # ReplicaDesired → docker convergence
│   │   └── volumes/     # named-volume + bind-mount pre-flight
│   └── scheduler/       # leader-only reconcile + rollout + drain + restart
│       ├── drain/       # graceful node remove
│       ├── health/      # restart-with-fail-after-3
│       ├── placement/   # spread / pack / hosts
│       ├── rebalance/   # pressure-based rebalancer (ADR 0002)
│       └── rollout/     # per-service rolling-update plan
├── pkg/                 # generated, exported packages
│   └── proto/jaco/v1/   # buf-generated gRPC + proto types
├── proto/               # proto source files
│   └── jaco/v1/         # entities, services, commands, errors, events, fsm
├── docs/                # operator + developer documentation tree
├── tests/               # end-to-end tests
│   ├── isolation/       # 3-node network-isolation rig (canonical privileged E2E)
│   ├── samples/         # comparative bench: JACO vs k8s/k3s/swarm
│   └── testbed/         # Azure provisioning template for benchmarking
├── testdata/            # repo-root testdata
├── scripts/test/        # privileged integration + isolation rig scripts
├── build/               # systemd unit, jacod.yaml template, install scripts
├── .github/workflows/   # ci / integration / isolation-rig / release
├── Makefile
├── nfpm.yaml            # deb/rpm/apk packaging recipe
├── buf.yaml, buf.gen.yaml # protobuf generation config
└── .golangci.yml        # linter config (correctness-only)
```

## Where to add what

- **New CLI subcommand** → `cmd/jaco/<name>.go`. Follow the shape in
  `apply.go` or `logs.go`: cobra command + `runX` body taking the
  proto client so tests can inject a fake.
- **New gRPC handler** → `internal/controlplane/grpc/<file>.go` plus
  the proto definition under `proto/jaco/v1/`. Regenerate with
  `make proto`.
- **New audit event type** → `proto/jaco/v1/entities.proto` (extend
  `AuditEventType`), then teach `internal/controlplane/grpc` to map
  it to/from the short-form string for the CLI.
- **New configuration key** → `internal/daemon/config/config.go`
  (typed field, default, validator) plus a doc update at
  [`docs/configuration.md`](../configuration.md).
- **New scheduler behavior** → `internal/scheduler/`. Always
  leader-only; gate on `leader.IsLeader()`.
- **New compose field support** → `internal/runtime/compose/validate.go`
  (`allowedServiceFields`) plus the mapping in
  `internal/runtime/compose/spec.go`.

## See also

- [Development](development.md)
- [Release and packaging](release-and-packaging.md)
- [Testing](testing.md)
