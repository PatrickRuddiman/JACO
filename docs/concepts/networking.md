# Networking

East-west traffic between containers in JACO uses three layers:

1. **Per-(deployment, network) docker bridges** on every node hosting
   a replica of any service attached to that network. One bridge per
   tuple, one `/24` per tuple.
2. **A WireGuard mesh** (`wg-jaco`) carrying inter-node container
   traffic encrypted.
3. **A per-bridge DNS responder** answering service-name lookups
   within each (deployment, network).

Cross-deployment and cross-network traffic is blocked at L3 by the
nftables ruleset described in [Isolation](isolation.md). This page is
about the connectivity plane; that page is about the deny plane.

Source-of-truth design:
[`slices/discovery.md`](../planning/slices/discovery.md). Code lives
under [`internal/discovery/`](../../internal/discovery).

## IPAM

The leader carves the cluster IPAM pool into `/24`s on demand, one
per `(deployment, network)` tuple. The pool is configured at the
daemon level via [`ipam_pool`](../configuration.md) and defaults to
`10.244.0.0/16` (256 `/24`s available).

Allocations are stored in raft as `Subnet{deployment, network, cidr,
host?}` entities. Pool exhaustion rejects the apply with
`subnet_pool_exhausted`. Subnets free on deployment delete or network
removal.

## Bridges

Naming:

- Linux bridge: `jaco-<dep-hash>-<net-hash>` where each hash is a
  4-char SHA-1 prefix (kernel limits bridge names to 15 chars).
- Docker network: `jaco_<deployment>_<network>` (no length limit).

Labels on every bridge:

- `jaco.deployment=<name>`
- `jaco.network=<name>`
- `jaco.cluster_id=<uuid>`
- `jaco.subnet=<cidr>`

Bridge IPAM: gateway is the first IP of the `/24`; container IPs are
allocated by docker's IPAM constrained to the subnet.

A service declared on `[frontend, backend]` attaches to both bridges
on its host (two interfaces, two IPs). A service with no `networks:`
attaches to the per-deployment `_default` bridge.

## WireGuard mesh

One `wg-jaco` interface per node, listening on UDP
[`wg_port`](../configuration.md) (default `51820`).

- Each node generates its private key once on first boot under
  `$JACO_DATA_DIR/wg.key`. The public key is published in
  `Node{wireguard_pubkey}` via raft.
- Peer entries are sourced from the `Node` watch: one entry per remote
  node, `endpoint = node.address:<wg_port>`,
  `allowed_ips = ∪{Subnet.cidr | subnet hosted on that peer}`.
- `AllowedIPs` is recomputed on every placement change so the peer
  set converges with replica movement.
- Persistent keepalive: 25 s on every peer (handles NAT traversal in
  cross-DC clusters).

Cross-node traffic on a same-(deployment, network) tuple flows
node-local-bridge → `wg-jaco` → peer's `wg-jaco` → peer's local-bridge
→ destination container.

## DNS responder

One responder per bridge, listening on the bridge gateway IP on UDP+TCP
port 53. Each responder is authoritative for the services attached to
its own (deployment, network) and forwards anything else to the node's
`/etc/resolv.conf` nameservers.

Container `/etc/resolv.conf` is set by JACO at container create:

- `nameserver <gateway-ip-of-first-bridge>` first, then any
  additional bridges' DNS in compose-declared attach order. First
  match wins.
- A query for `<service>` or `<service>.jaco.local` returns A records
  for every healthy `ReplicaObserved` of that service on this
  (deployment, network), in random order across the cluster.
- A query for a service that isn't in this (deployment, network)
  returns NXDOMAIN.

A multi-network service (gateway pattern) sees both DNS responders;
the libc resolver tries them in attach order and the first to answer
wins. This is the standard docker behavior.

## Cross-node reachability (concrete example)

Replicas `web-api-0` on node A and `web-api-1` on node B, both
attached to deployment `web`'s network `frontend` (subnet
`10.244.5.0/24`):

1. `web-api-0` on A has IP `10.244.5.2`; `web-api-1` on B has
   `10.244.5.3`. Same `/24`; the cluster shares one subnet per
   (deployment, network).
2. A's WG peer entry for B includes `10.244.5.0/24` in `allowed_ips`;
   same for B's peer for A.
3. A container on A sends a packet to `10.244.5.3`. Node A routes via
   `wg-jaco` to node B; node B's WG decrypts and delivers to the local
   `jaco-web-frontend` bridge.
4. nftables FORWARD on node B matches: source and destination are
   both in the `dep_net_web_frontend` set → ACCEPT.

## Kernel gates

The WG mesh, the nftables firewall, and the per-bridge DNS responder
each require kernel facilities (`CONFIG_WIREGUARD`, an `nft` binary,
`CAP_NET_ADMIN`). When a host is missing one, the daemon logs a
one-line warning and continues **without that subsystem**. The
scheduler, runtime, and ingress paths work either way; cross-host
container traffic and isolation are degraded or absent.

For production multi-host clusters, every node SHOULD have all three.
For single-host dev / CI usage, the kernel gates are typically already
satisfied by stock Linux.

## What's IPv4-only

v1 is IPv4-only. The IPAM pool is a single IPv4 `/16`; the WG mesh
ferries IPv4; the nftables ruleset is `ip` family. IPv6 is explicitly
out of scope.

## See also

- [Isolation](isolation.md)
- [Ingress](ingress.md)
- [Configuration](../configuration.md) — `ipam_pool`, `wg_port`
- [`slices/discovery.md`](../planning/slices/discovery.md)
