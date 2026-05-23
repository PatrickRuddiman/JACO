# JACO — Just Another Container Orchestrator

JACO is a multi-node container orchestrator built on hashicorp/raft,
embedded Caddy, WireGuard, and per-(deployment, network) bridges with
nftables-enforced isolation. It ships as two binaries: `jacod` (the
long-running daemon, managed by systemd) and `jaco` (the operator CLI
that talks to a local jacod over a unix socket and to peer jacods over
TCP for cross-host control).

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

## Install

From a release tarball:

```sh
tar xf jaco-vX-linux-amd64.tar.gz
cd jaco-vX-linux-amd64
sudo ./install.sh
sudo systemctl enable --now jacod
```

The installer drops `/usr/local/bin/{jaco,jacod}`, a default
`/etc/jaco/jacod.yaml` (edit `listen_addr` / `cluster_addr` / `data_dir`
as needed), and the `jacod.service` systemd unit. The daemon comes up in
the uninitialized state — every RPC except `Cluster.{Init,Join,Status}`
returns `cluster_uninitialized` until one of those two transitions runs.

## Bring up a cluster

On the first node:

```sh
sudo jaco cluster init
# prints: cluster_id=… operator_token=<64 hex chars> — save the token
```

Mint a single-use 24h join token on the leader (operator-authenticated):

```sh
JACO_TOKEN=<operator_token> jaco node issue-join-token
# prints: token=… leader_addrs=…
```

On each follower (gRPC port defaults to `7000`, see
`/etc/jaco/jacod.yaml::listen_addr`):

```sh
sudo jaco node join --peer <leader-host>:7000 --token <single-use>
```

After all nodes report `READY` in `jaco node list`, ship a deployment.
Every operator-facing command needs `--server <any-node>:7000` (the
gRPC port from `/etc/jaco/jacod.yaml::listen_addr`) plus the operator
token:

```sh
export JACO_TOKEN=<operator_token>
export LEADER=<leader-host>:7000

jaco apply  --server $LEADER path/to/jaco.yaml
jaco status --server $LEADER my-deployment           # -w to follow
jaco logs   --server $LEADER my-deployment/web --follow
jaco node   list --server $LEADER
```

## Network model

The daemon's cross-host gRPC listener (`listen_addr`) and raft transport
(`cluster_addr`) are plaintext TCP in v0 — JACO assumes the wire is
wrapped by Tailscale / WireGuard / your own overlay. Cluster
authentication still relies on the operator bearer token plus the
single-use join token; only the wire confidentiality is delegated.
TLS-with-cluster-CA + cert pinning lands in a follow-up.

Discovery subsystems (WireGuard mesh, nftables firewall, per-bridge DNS)
are kernel-gated — when the host lacks the relevant kernel feature
(unprivileged container, missing `CONFIG_WIREGUARD`, no `nft` binary),
the daemon logs a one-line warning and proceeds without that subsystem.
The scheduler + runtime + ingress paths work either way.

## Status

Pre-release. Functional for single-host and multi-host clusters via the
two-binary path described above. Open gaps: TLS on the cross-host
listener, follower→leader forwarding of `ReplicaObserved` updates, the
caddy/v2 ingress reload loop, rollout state-machine integration with the
scheduler, and the drain step machine for `jaco node remove`.

See `spec.md` for the v1 contract and `design.md` for the architecture
overview.
