---
sources:
  - internal/discovery/bridge/
  - internal/discovery/ipam/
  - internal/discovery/wgmesh/
  - internal/discovery/dns/
  - internal/discovery/runtime_attach/
---

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

Code lives under [`internal/discovery/`](../../internal/discovery).

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

One responder per bridge, listening on the bridge gateway IP on
UDP+TCP port 53. Each responder is authoritative for the services
attached to its own (deployment, network) and forwards anything else
to an upstream chain via the per-daemon **forwarder** (see below).

Docker writes the container's `/etc/resolv.conf` to point at its
embedded resolver `127.0.0.11`, which in turn forwards to the
bridge gateway IP listed in `ExtServers`. So the path is:

```
container → 127.0.0.11 (docker)
         → 10.244.<n>.1 (bridge gateway = jacod responder)
         → forwarder chain → external upstream
```

A query for `<service>`, `<service>.<deployment>`, or
`<service>.jaco.internal` returns A records for every healthy
`ReplicaObserved` of that service in the responder's scope, in
random order. A query for a service that isn't in this
(deployment, network) but IS a single bare label returns NXDOMAIN
(in-scope but unknown). Anything else with a dot is treated as
external and handed to the forwarder.

## Forwarder

External-name lookups go through
[`internal/discovery/dns/forwarder.go`](../../internal/discovery/dns/forwarder.go),
a `miekg/dns` client driven against an explicit upstream chain:

- A and AAAA legs run in parallel.
- Each upstream is tried with a per-upstream deadline (default 2 s
  via `DefaultDNSForwarderTimeout`); transient errors (transport
  failures, `SERVFAIL`, `REFUSED`, `NOTIMP`) fall through to the
  next upstream.
- The first authoritative answer (`NOERROR` or `NXDOMAIN`,
  including empty) wins; the chain stops there.
- The full upstream list is bounded; if every upstream fails the
  call returns an error and the responder surfaces **`SERVFAIL`**
  to the downstream resolver (NOT `NXDOMAIN` — downstreams
  negative-cache the latter, breaking the name for the TTL window
  even when the upstream recovers). Issue #165.

The upstream list comes from `jacod.yaml`'s
[`dns.forwarders`](../configuration.md#dns) when set, otherwise the
daemon parses `/etc/resolv.conf` at startup and uses every
`nameserver` entry it finds. Two addresses are filtered out of the
host fallback to avoid forwarding loops:

- `127.0.0.11` — Docker's embedded resolver. Container DNS reaches
  the bridge responder THROUGH this address; configuring it as our
  upstream would loop forever.
- `10.244.*.1` — every JACO bridge gateway. Same loop risk.

The same two are rejected at config-validate time when the operator
specifies them under `dns.forwarders` (loud startup error rather
than a silent loop). See
[`ValidateUpstreams`](../../internal/discovery/dns/forwarder.go).

When the operator hasn't set `dns.forwarders` AND
`/etc/resolv.conf` yields no usable nameservers, the daemon logs a
single startup warning and the responder `SERVFAIL`s every external
query. The bridge resolver still answers internal names; only
external lookups fail. Set `dns.forwarders` explicitly to fix.

A multi-network service (gateway pattern) sees both DNS responders;
the libc resolver tries them in attach order and the first to
answer wins. Standard docker behavior.

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
