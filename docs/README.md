---
sources:
  - README.md
---

# JACO documentation

JACO — Just Another Container Orchestrator. Multi-node, Docker-engine,
raft-replicated, embedded Caddy, WireGuard mesh, nftables-enforced
isolation. Two binaries: `jacod` (daemon) and `jaco` (CLI).

This tree is the user-facing documentation.

## Start here

- [Getting started](getting-started.md) — install on three hosts, form a
  cluster, ship a deployment, read logs. End-to-end in one page.
- [Installation](installation.md) — per-distro packages, the generic
  tarball, verification, on-disk layout.
- [Configuration](configuration.md) — `/etc/jaco/jacod.yaml`: every
  key, its default, and what changes when you set it.

## CLI reference

Every `jaco` subcommand has a dedicated page with synopsis, flags,
auth, behavior, exit codes, and examples. See [`cli/README.md`](cli/README.md)
for the index and the global flags shared by every command.

## Manifests

JACO consumes a pair of files per deployment: your existing
`docker-compose.yml` plus a small `jaco.yaml` overlay declaring replica
counts, placement, and routes.

- [`jaco.yaml` schema](manifests/jaco-yaml.md) — closed schema:
  `deployment`, `services`, `routes`.
- [Supported compose fields](manifests/compose.md) — what JACO honors,
  ignores, and rejects.
- [Examples](manifests/examples.md) — progressive samples from one
  service to multi-network with routed ingress.

## Concepts

Why each subsystem is shaped the way it is.

- [Architecture](concepts/architecture.md) — the two binaries, the
  verticals, the project status.
- [Cluster lifecycle](concepts/cluster-lifecycle.md) — bootstrap, join,
  leader election, graceful remove.
- [Networking](concepts/networking.md) — WireGuard mesh,
  per-(deployment, network) bridges, /24 IPAM, DNS.
- [Isolation](concepts/isolation.md) — nftables ruleset, cross-deployment
  DROP, ready gate.
- [Ingress](concepts/ingress.md) — embedded Caddy, ACME, HTTP-01
  challenge coordination, L4 ports.
- [Scheduling](concepts/scheduling.md) — placement modes, rolling
  updates, restart policy.
- [Auth and tokens](concepts/auth-and-tokens.md) — operator tokens, join
  tokens, the unix-socket trust boundary.
- [Status and errors](concepts/status-and-errors.md) — closed enums,
  replica states, error codes.
- [Observability](concepts/observability.md) — OTel exporter env, span
  names, metrics, logs.

## Operations

- [Migration](operations/migration.md) — move an existing
  docker-compose stack (with volumes) onto a JACO cluster.
- [Upgrades](operations/upgrades.md) — rolling `jaco self-upgrade`
  walkthrough.
- [Backups](operations/backups.md) — `jaco backup` and `jaco restore`
  end-to-end.
- [Recovery](operations/recovery.md) — quorum loss, node loss,
  partitions, isolation drift.
- [Troubleshooting](operations/troubleshooting.md) — the error codes you
  will actually hit and how to clear them.

## Contributing

- [Repository layout](contributing/repo-layout.md) — what lives where.
- [Development](contributing/development.md) — `make build/test/vet/lint`,
  proto generation, working with `internal/`.
- [Release and packaging](contributing/release-and-packaging.md) — how
  releases are cut, signed, and published.
- [Testing](contributing/testing.md) — unit, integration, the privileged
  isolation rig, and the comparative samples bench.
- [Architecture decision records](adr/README.md) — load-bearing design
  decisions for multi-PR efforts (volume migration, pressure-based
  scheduling, orchestrator benchmark).

## License

JACO is licensed under the [Apache License 2.0](../LICENSE). Attribution
notices for bundled dependencies are in [`NOTICE`](../NOTICE), and the
full per-module third-party inventory (generated from `go.mod`) is in
[`THIRD_PARTY_LICENSES.md`](../THIRD_PARTY_LICENSES.md).
