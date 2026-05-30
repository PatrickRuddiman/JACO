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

## Installation

Releases are published at
<https://github.com/PatrickRuddiman/JACO/releases/latest>. Each release
ships `.deb`, `.rpm`, `.apk`, and a generic `.tar.gz` for `linux/amd64`
and `linux/arm64`, plus a `SHA256SUMS` manifest.

Pick the snippet that matches your distro and swap `<arch>` for either
`amd64` or `arm64`.

### Debian / Ubuntu

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_<arch>.deb
sudo dpkg -i jaco_<arch>.deb
sudo systemctl enable --now jaco
```

### RHEL / Fedora / CentOS

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_<arch>.rpm
sudo rpm -i jaco_<arch>.rpm        # or `sudo dnf install ./jaco_<arch>.rpm`
sudo systemctl enable --now jaco
```

### Alpine

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_<arch>.apk
sudo apk add --allow-untrusted ./jaco_<arch>.apk
```

> Alpine uses OpenRC, not systemd. The package installs the binaries
> and config; bring `jacod` up under your service manager of choice.

### Generic tarball

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco-vX.Y.Z-linux-<arch>.tar.gz
tar xf jaco-vX.Y.Z-linux-<arch>.tar.gz
cd jaco-vX.Y.Z-linux-<arch>
sudo install -m 0755 jaco  /usr/local/bin/jaco
sudo install -m 0755 jacod /usr/local/bin/jacod
sudo install -d -m 0755 /etc/jaco
sudo install -m 0644 jacod.yaml /etc/jaco/jacod.yaml
sudo install -m 0644 jaco.service /lib/systemd/system/jaco.service
sudo systemctl daemon-reload
sudo systemctl enable --now jaco
```

### Verify the download

`SHA256SUMS` lists every artifact in the release. The exact filename
pattern is what nfpm / the release workflow emits — for example
`jaco_0.1.0_amd64.deb`, `jaco-0.1.0-1.x86_64.rpm`,
`jaco_0.1.0_x86_64.apk`, `jaco-v0.1.0-linux-amd64.tar.gz`.

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```

After install, the layout is:

- `/usr/local/bin/{jaco,jacod}` — the CLI client and daemon.
- `/etc/jaco/jacod.yaml` — daemon config (edit `listen_addr` /
  `cluster_addr` / `data_dir` before first start).
- `/lib/systemd/system/jaco.service` — the systemd unit (daemon-reload
  runs automatically on package install).

The daemon comes up in the uninitialized state — every RPC except
`Cluster.{Init,Join,Status}` returns `cluster_uninitialized` until one
of those two transitions runs.

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

The cross-host **gRPC control plane** (`listen_addr`, default `:7000`)
runs over **TLS**: each daemon presents a node certificate signed by the
cluster CA, and the CLI and peer daemons verify against that CA (cert
pinning). The operator bearer token plus the single-use join token still
authenticate the caller on top of the transport.

The **raft transport** (`cluster_addr`, default `:7001`) is still
plaintext TCP — run it over a private network or overlay you control. A
few bootstrap hops (a node join before it holds the CA, and some
follower→leader forwarding) negotiate TLS without verifying the peer.

Discovery subsystems (WireGuard mesh, nftables firewall, per-bridge DNS)
are kernel-gated — when the host lacks the relevant kernel feature
(unprivileged container, missing `CONFIG_WIREGUARD`, no `nft` binary),
the daemon logs a one-line warning and proceeds without that subsystem.
The scheduler + runtime + ingress paths work either way.

## Status

Tagged releases through `v0.1.2`, functional for single-host and
multi-host clusters via the two-binary path described above. The earlier
gaps — cross-host gRPC TLS, follower→leader forwarding of
`ReplicaObserved` updates, the Caddy ingress reload loop, rollout
state-machine integration with the scheduler, and the drain step machine
for `jaco node remove` — are implemented. Known remaining item: the raft
transport is still plaintext (see Network model).

For operator and developer documentation see [`docs/`](docs/) —
start with [`docs/getting-started.md`](docs/getting-started.md) for the
end-to-end user path and
[`docs/concepts/architecture.md`](docs/concepts/architecture.md) for
the architecture overview.

## License

JACO is licensed under the [Apache License 2.0](LICENSE). Third-party
modules bundled with the binary are listed in
[`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md); upstream
attribution notices are aggregated in [`NOTICE`](NOTICE).
