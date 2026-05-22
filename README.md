# JACO — Just Another Container Orchestrator

JACO is a self-contained, single-binary container orchestrator built on
hashicorp/raft, embedded Caddy, WireGuard, and per-(deployment, network)
bridges with nftables-enforced isolation.

## Components

- **Control plane** — Raft-replicated state machine; gRPC API (Cluster,
  Tokens, Audit, Deploy, Watch); on-disk snapshots survive restarts.
- **Scheduler** — Leader-only reconcile loop with spread / pack / hosts
  placement, rolling updates with replicas-1 invariant, restart policy.
- **Runtime** — Docker engine driver; per-replica health watcher; image
  pull with exponential backoff; orphan reconcile on daemon boot.
- **Discovery** — Per-(deployment, network) docker bridges, deterministic
  /24 IPAM, WireGuard mesh, nftables east-west isolation, per-bridge DNS.
- **Ingress** — Embedded Caddy v2 reverse-proxy; per-route ACME via raft-
  backed CertMagic storage; HTTP-01 challenge coordination through raft.

## Quick start

`jaco bootstrap` brings the first node up and prints an operator token.
`jaco node issue-join-token` produces a single-use 24h token for the next
node; `jaco node join` adds it to the cluster. `jaco apply` ships a
deployment defined by a compose file + a JACO manifest.

For installation, run `bash install.sh` from a release tarball as root.

## Status

Pre-release. The cross-cutting daemon entry that ties the slices together
is in progress — most slice primitives (control plane, scheduler,
runtime, discovery, ingress) ship with comprehensive `-race` unit tests
in the meantime.

See `spec.md` for the v1 contract and `design.md` for the architecture
overview.
