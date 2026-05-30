# Getting started

Install JACO on three Linux hosts, form a cluster, and ship a deployment.
End-to-end in one page.

## Prerequisites

- Three Linux hosts with Docker Engine ≥ 24.0, reachable on a private
  network (LAN, VPC, Tailscale, or any other overlay).
- Each host needs `CAP_NET_ADMIN` plus a kernel with WireGuard and
  `nft` installed. The `.deb` / `.rpm` packages depend on a Docker
  package that satisfies the engine requirement.
- TCP `7000` (cluster gRPC) and `7001` (raft) reachable between nodes;
  UDP `51820` for the WireGuard mesh. TCP `80` and `443` reachable from
  whichever clients hit your ingress.

The cross-host gRPC control plane (`:7000`) runs over **TLS** with the
cluster CA — the CLI and peer nodes pin it — and the operator bearer
token authenticates the caller on top. The **raft transport** (`:7001`)
is still plaintext TCP, so run it over a private network or overlay you
control. See [Networking](concepts/networking.md) and the README
"Network model" section.

## 1. Install on every host

Pick the snippet for your distro from [Installation](installation.md).
The Debian path looks like:

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_amd64.deb
sudo dpkg -i jaco_amd64.deb
sudo systemctl enable --now jaco
```

After install the daemon is **uninitialized** — every RPC except
`Cluster.{Init,Join,Status}` returns `cluster_uninitialized` until one of
the next two steps runs. Confirm with `jaco cluster status` on each node.

## 2. Initialize the first node

On node 1:

```sh
sudo jaco cluster init
# Cluster initialized.
#   cluster_id:     <uuid>
#   operator_token: <64 hex chars>
#
# Save the operator token now — it cannot be recovered.
```

Save the operator token in your password manager. It is the only
credential that authorizes state-changing RPCs against this cluster
until you issue more via `jaco token issue`.

## 3. Join the other nodes

On node 1, mint a single-use 24-hour join token:

```sh
export JACO_TOKEN=<operator_token>
jaco node issue-join-token
# Join token issued. On the joining node, run:
#
#   sudo jaco node join --peer=<node-1-host:port> --token=<single-use>
#
# Token expires in 24h (single-use).
```

On each follower:

```sh
sudo jaco node join --peer <node-1-host>:7000 --token <single-use>
# Joined cluster.
```

Confirm everyone is in:

```sh
export LEADER=<node-1-host>:7000
jaco node list --server $LEADER
```

Wait for every node to show `READY`. The cross-host gRPC port comes from
`/etc/jaco/jacod.yaml::listen_addr` and defaults to `7000`. See
[Configuration](configuration.md).

## 4. Ship a deployment

You need two files in a directory: `jaco.yaml` and a `docker-compose.yml`.
JACO consumes the compose file unmodified; the `jaco.yaml` overlay declares
replica counts, placement, and ingress routes.

`./hello/jaco.yaml`:

```yaml
deployment: hello
services:
  - name: web
    replicas: 3
routes:
  - domain: hello.example.com
    service: web
    port: 80
    tls: auto
```

`./hello/docker-compose.yml`:

```yaml
services:
  web:
    image: nginx:1.27
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1/"]
      interval: 5s
      timeout: 3s
      retries: 5
```

Apply from any node (the CLI dials the leader for you):

```sh
jaco apply --server $LEADER ./hello/jaco.yaml
# Applied revision: 1
```

JACO pulls the image on each scheduled host, starts containers, attaches
them to the per-(deployment, network) bridge, registers the route with
the embedded Caddy, and queues ACME issuance for `hello.example.com`.

## 5. Watch the rollout

```sh
jaco status --server $LEADER hello -w
```

The `-w` flag re-renders on every state change. You'll see three replicas
move through `pending → pulling → running`, and a `Routes` row land for
`hello.example.com`. Cert state shows up under `Certs` once ACME succeeds
(DNS for the domain must resolve to your nodes for that to happen).

Stream logs from all replicas across every node:

```sh
jaco logs --server $LEADER hello/web --follow
```

## 6. Roll an update

Edit `./hello/docker-compose.yml`, bump `image:` to `nginx:1.28`, and
re-apply:

```sh
jaco apply --server $LEADER ./hello/jaco.yaml
```

The scheduler replaces replicas one at a time, never dropping below
`replicas - 1` running. `jaco status hello -w` shows the rollout state.
If something looks wrong, roll back to the previous revision:

```sh
jaco rollback --server $LEADER hello
```

## What to read next

- [CLI reference](cli/README.md) — every subcommand, every flag.
- [`jaco.yaml` schema](manifests/jaco-yaml.md) and [supported compose
  fields](manifests/compose.md) — what's actually accepted.
- [Architecture](concepts/architecture.md) — the moving parts and the
  current status.
- [Troubleshooting](operations/troubleshooting.md) — the error codes you
  will hit and how to clear them.

## See also

- [Installation](installation.md)
- [Configuration](configuration.md)
- [Manifest examples](manifests/examples.md)
