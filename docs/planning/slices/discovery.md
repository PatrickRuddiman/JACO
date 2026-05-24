Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — discovery

## §1 Summary

Per-node east-west traffic plane. Owns docker bridges per (deployment, network), CIDR allocation, IP address management within each bridge, WireGuard mesh maintenance for cross-node container traffic, nftables rules enforcing deployment + network isolation, and the DNS responder that resolves service names to healthy replica IPs within each isolated network.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **DNS server library.** Options: `miekg/dns`, embed CoreDNS, custom. **Chosen:** `miekg/dns`. Rationale: the standard Go DNS library; what CoreDNS and consul use; only need ~10 query types handled.
2. **WireGuard control library.** Options: `wgctrl-go`, shell out to `wg`+`ip`, embedded wireguard-go userspace. **Chosen:** `wgctrl-go` against kernel WireGuard via netlink. Rationale: stable, no fork/exec, fail-fast errors; JACO daemon already needs CAP_NET_ADMIN.
3. **IPAM model.** Options: per-(deployment, network) /24 from cluster pool, /28 from /16, per-node /24 with sub-allocation. **Chosen:** /24 per (deployment, network) from cluster `10.42.0.0/16`. Rationale: simplest routing model; one bridge owns one /24; WG `AllowedIPs` per peer is a clean union of /24s.
4. **Bridge model.** Options: single bridge per node, per-deployment bridge, per-(deployment, network) bridge. **Chosen:** one docker bridge per (deployment, network) per node. Rationale: gives L2 separation matching compose `networks:` semantics + cross-deployment isolation. Required by spec §3 In additions.
5. **Isolation enforcement.** Options: L2 bridge separation only, nftables on top, eBPF. **Chosen:** L2 bridge separation + JACO-managed nftables rules at the FORWARD chain. Rationale: bridges alone don't stop a container with raw socket privileges or a misconfigured route; nftables FORWARD ensures cross-bridge or cross-node traffic between different (deployment, network) tuples is dropped.
6. **nftables rule matching strategy.** Options: interface-name matching (`iifname`/`oifname`), IP-set matching, hybrid. **Chosen:** IP-set matching with named sets per (deployment, network). Rationale: cross-node traffic arrives via `wg-jaco` not a JACO bridge, so interface matching alone misses it; IP-set matching works uniformly for same-node and cross-node paths.
7. **Ruleset reload atomicity.** Options: incremental rule add/delete, full ruleset transactional reload, hybrid. **Chosen:** full ruleset rendered and reloaded as one transaction via `nft -f -`. Rationale: incremental updates can briefly leave the table in an inconsistent state (rules half-deleted); the spec requires no isolation gap; full reload is cheap (sub-ms for our rule cardinality).
8. **Docker iptables coexistence.** Options: disable docker iptables, dual-stack iptables+nftables, nftables in a separate table. **Chosen:** leave docker's iptables management enabled (it handles NAT for outbound container traffic and per-bridge defaults); JACO rules live in a separate nftables table `inet jaco` whose DROP outcomes apply regardless of docker's parallel chains. Rationale: disabling docker iptables breaks outbound NAT for containers — out of scope to replace; parallel chains compose correctly because any DROP stops the packet.
9. **Multi-network DNS resolver order in containers.** Options: first-declared network's resolver only, all networks' resolvers in attach order, all networks' resolvers in lexicographic network name order. **Chosen:** all networks' resolvers in compose-declared attach order. Rationale: deterministic; matches docker's default behavior when a container is on multiple networks; first match wins so `<service>` resolves on the first network that has it.

## §4 Contracts & shapes

Module layout under `internal/discovery/`:

- `internal/discovery/discovery.go` — boots at daemon start; opens watches on Deployments, Subnets, ReplicaObserved, Nodes; coordinates the four sub-modules below.
- `internal/discovery/ipam.go` — leader-only Subnet allocator: on a new (deployment, network) reference, picks the next free /24 from the cluster pool and writes `Subnet{deployment, network, cidr}` via raft Apply.
- `internal/discovery/bridge.go` — per-node: ensures a docker bridge named `jaco-<deployment>-<network>` exists for every (deployment, network) the node hosts a replica for; tears down bridges with no remaining replicas.
- `internal/discovery/wireguard.go` — per-node: maintains the WG interface (`wg-jaco`), peer table, allowed-IPs per peer (union of subnets that peer hosts), private key on disk at `$JACO_DATA/wg.key`, public key published to `Node{wireguard_pubkey}` via raft on join.
- `internal/discovery/firewall.go` — per-node: manages nftables table `jaco`, chains `forward` and `input`, rules that ACCEPT traffic with `(in_bridge_label.deployment, in_bridge_label.network) == (out_bridge_label.deployment, out_bridge_label.network)` and DROP otherwise.
- `internal/discovery/dns.go` — per-node DNS responder on each bridge's `.1` IP, port 53. Answers from a watch-fed cache filtered to the (deployment, network) of the bridge it's serving.
- `internal/discovery/runtime_attach.go` — small helper called by runtime slice when creating a container: returns the list of bridges to attach to based on compose `networks:` declarations.

Subnet allocator rules:

- Cluster pool default `10.42.0.0/16` → 256 /24s available.
- `_default` per deployment counts as one allocation.
- Allocator picks the lowest-numbered free /24. Frees on `Deployment` delete or `network` removal from compose.
- Allocation is a raft Apply (single-leader, no contention). Exhaustion of the pool causes apply to fail with `Error{code: subnet_pool_exhausted}`.

Bridge naming + labels:

- Linux bridge name: `jaco-<dep-hash>-<net-hash>` where each hash is the 4-char hex SHA-1 prefix of the name (kernel limit is 15 chars).
- Docker network name: `jaco_<deployment>_<network>` (full names; docker has no 15-char limit).
- Each bridge carries labels `jaco.deployment=<name>`, `jaco.network=<name>`, `jaco.cluster_id=<uuid>`, `jaco.subnet=<cidr>`.
- Bridge IPAM: gateway = `<cidr first IP>` (e.g. `10.42.0.1` for `10.42.0.0/24`); container IPs from `.2` upward, allocated by docker IPAM constrained to the subnet.

DNS responder behavior (per bridge):

- Listens on `<bridge_gateway_ip>:53` UDP+TCP.
- Authoritative for `<service>.jaco.local` and bare `<service>` within the (deployment, network) of that bridge.
- Returns A records for every healthy `ReplicaObserved` of `<service>` attached to this network across the cluster; multi-record response, random order.
- Returns NXDOMAIN for any `<service>` not in this deployment OR not on this network.
- Forwards everything else (external hostnames) to the node's `/etc/resolv.conf` nameservers.

WireGuard interface contract:

- One `wg-jaco` interface per node, listening on UDP `:51820` (configurable).
- Peer entries: one per remote `Node` in raft state; `endpoint = node.address:51820`; `allowed_ips = ∪ {Subnet.cidr | subnet is hosted on that node}`.
- Recompute `allowed_ips` on every replica placement change so the peer set converges.
- Persistent keepalive: 25s on every peer (NAT traversal in cross-DC clusters).

nftables ruleset (managed under table `inet jaco`, rendered and applied as one transaction on every relevant change):

- One **named set** per (deployment, network): `set dep_net_<dep>_<net> { type ipv4_addr; flags interval; elements = { <subnet_cidr> }; }`. Set names are sanitized to nftables identifier rules; long names hashed.
- `chain forward { type filter hook forward priority 0; policy drop; }` containing in order:
  1. `ct state established,related accept;` — return path for already-allowed flows.
  2. For each set X: `ip saddr @<X> ip daddr @<X> accept;` — same-(deployment, network) traffic anywhere in the cluster, regardless of whether iifname is a JACO bridge or `wg-jaco`.
  3. Final implicit DROP (chain policy).
- `chain input { type filter hook input priority 0; policy drop; }` containing:
  1. `iifname "lo" accept;`
  2. `udp dport 51820 accept;` — WireGuard ingress.
  3. `iifname "wg-jaco" accept;` — already filtered upstream; allow node-host services to receive WG traffic when needed.
  4. `iifname starts_with "jaco-" udp dport 53 ip daddr @local_dns_anchors accept;` — DNS responder reachable from JACO bridges.
  5. `tcp dport 7000 accept;` — JACO gRPC API.
  6. `tcp dport {80, 443} accept;` — public ingress (Caddy listens on these).
  7. Final implicit DROP.
- `chain output { type filter hook output priority 0; policy accept; }` — JACO does not constrain egress from the host itself.
- Atomic apply: JACO renders the full ruleset text into a temp file, then runs `nft -f <file>`. `nft` is invoked via `os/exec` because `wgctrl`-style golang bindings for nftables are less mature; `github.com/google/nftables` is a viable alternative but adds dep weight.
- Self-test on startup: after first load, JACO runs `nft list ruleset` and checks the table structure matches expectations; emits `Error{code: isolation_self_test_failed}` on mismatch and refuses to enter ready state.

## §5 Sequence

Daemon startup on each node:

1. `jaco serve` constructs `Discovery`; reads or generates `$JACO_DATA/wg.key`.
2. On first start: writes the public key to `Node{wireguard_pubkey}` via raft.
3. Opens watches on Nodes, Subnets, ReplicaObserved.
4. Brings up `wg-jaco` interface with current peer set.
5. Brings up nftables ruleset.
6. Initial reconcile: for every (deployment, network) where a replica on this node is desired, ensure bridge + DNS responder.

New deployment apply (leader):

1. Scheduler observes new Deployment; computes ReplicaDesired for each service.
2. Before writing ReplicaDesired, scheduler calls discovery's `EnsureSubnets(deployment, networks)` which, for any (deployment, network) without a Subnet entry, raft-Applies a new `Subnet`.
3. Watch fires on every node; bridge module on nodes that will host replicas of that deployment creates bridges.
4. DNS responder starts for each new bridge.
5. Scheduler writes ReplicaDesired; runtime attaches containers to the appropriate bridges.

Container attaches to bridge:

1. Runtime slice's create-container path calls `discovery.runtime_attach.BridgesForService(deployment, service)`.
2. Discovery looks up the service's `networks:` declaration in the compose; if none, returns the deployment's `_default` bridge. Else returns one bridge per declared network.
3. Runtime creates the container with `NetworkMode = "none"` initially, then attaches it to each bridge via `NetworkConnect` (multi-network attach).
4. Container's `/etc/resolv.conf` is set by JACO at creation: nameserver `<gateway-ip-of-first-bridge>`; subsequent bridges' DNS responders also answer queries for their own networks (containers reach them via routing).

Cross-node replica reachability:

1. Replica `web-api-0` on node A, network `frontend`, subnet `10.42.5.0/24`, container IP `10.42.5.2`.
2. Replica `web-api-1` on node B, same network, container IP `10.42.5.3`.
3. Node A's WG peer entry for B includes `10.42.5.0/24` in `allowed_ips`. Same for Node B's peer for A.
4. Container on A sends packet to `10.42.5.3`; node A routes via `wg-jaco` to node B; node B's WG decapsulates and delivers to local bridge `jaco-web-frontend`.
5. nftables FORWARD chain matches: same (deployment, network) tuple, ACCEPT.

Cross-deployment block:

1. Container in deployment `front` (subnet `10.42.1.0/24`) attempts to connect to container in deployment `back` (subnet `10.42.7.0/24`).
2. DNS: source container's resolver answers from its bridge's DNS responder, which only knows services on its own (deployment, network); query returns NXDOMAIN immediately.
3. Even if the source guesses the destination IP (env-var leak, port scan, hardcoded config): packet enters FORWARD. No named set contains both `10.42.1.x` and `10.42.7.x`. Final DROP fires.
4. Same path applies to cross-node attempts: WG decrypts the packet, it lands in FORWARD with no matching set, DROP.

Cross-network within deployment:

1. Service `web` on network `frontend` (subnet `10.42.3.0/24`), service `db` on network `backend` (subnet `10.42.4.0/24`), same deployment.
2. `web` resolves `db`: its resolver lookup runs against `frontend`'s DNS responder (the only one reachable from `web`'s interface), which does not know about `backend`-only services. NXDOMAIN.
3. By-IP attempt: FORWARD has sets `dep_net_<dep>_frontend = {10.42.3.0/24}` and `dep_net_<dep>_backend = {10.42.4.0/24}`. No single set contains both. DROP.

Multi-network service (gateway case):

1. Service `gateway` declared on `[frontend, backend]` in compose; replica attached to both bridges; has two interfaces and two IPs.
2. `gateway` resolving `db` (on `backend` only): resolver list is `[frontend_dns, backend_dns]` in attach order; `frontend_dns` answers NXDOMAIN; `backend_dns` answers with `db`'s IP. (Standard libc resolver tries the next entry on NXDOMAIN.)
3. Outbound route to `db`'s IP `10.42.4.5`: container's routing table has `10.42.4.0/24` via the backend interface; packet egresses there.
4. FORWARD match: source IP in `dep_net_<dep>_backend`, dest IP in `dep_net_<dep>_backend` → ACCEPT.

Isolation ruleset reconcile loop (30s safety tick):

1. Periodic check: `nft list table inet jaco` and diff against the expected rendered ruleset.
2. On any drift (operator manually edited, another tool clobbered the table, transient kernel issue):
   - Re-render the expected ruleset.
   - `nft -f` atomic reload.
   - Emit `AuditEvent{type: isolation_ruleset_reconciled, identity: system, payload: {node, diff}}`.
3. If reload fails: transition node state to `isolation_unavailable`; cease accepting new container creates; existing containers continue running (we don't tear them down — that would amplify damage).

Isolation startup failure:

1. Daemon boots; discovery initializes.
2. nftables load fails (no kernel module, no `nft` binary in PATH, transaction error).
3. Daemon emits `Error{code: isolation_self_test_failed}` to logs + audit.
4. Daemon does NOT call `sd_notify(READY=1)` — systemd considers the unit failed/starting.
5. `Node.status` in raft state stays `joining` until isolation comes up; other nodes see this and do not schedule replicas here.
6. Operator investigates (probably missing nftables, missing CAP_NET_ADMIN, or kernel <4.14). After fix, `systemctl restart jaco` retries.

Compose network validation:

1. `Deploy.Apply` handler invokes compose-go parser.
2. Validator checks: every service-level `networks:` entry refers to a key in the top-level `networks:` block.
3. On mismatch: rejects with `Error{code: unknown_network, message: "unknown network: <name>; declared: [a, b]"}` and no state changes.

Node join:

1. New node B comes up; brings up `wg-jaco`; publishes its pubkey.
2. Existing nodes' wireguard module sees `Event<Node>{Added}`; adds B to their WG peer table with empty `allowed_ips` initially.
3. As B starts hosting replicas, `Subnet`s it hosts get added to its peer entries on every other node.

Node leave (graceful remove):

1. Scheduler reschedules B's replicas (drain slice in scheduler).
2. After B has no remaining replicas, its peer entries on other nodes have empty `allowed_ips`.
3. `Cluster.NodeRemove` raft-removes B; watch fires; wireguard module on every other node removes B from peer table.

## §6 Test rig (mandatory before this slice is called done)

This slice owns a v1 user-visible promise (cross-deployment + cross-network isolation). The promise must be testable end-to-end in CI, not just unit-tested. Required test surface:

- **Positive (must succeed)**: same-deployment-same-network TCP connect across nodes; same case for UDP; DNS resolution succeeds for an in-network service.
- **Negative (must fail)**: cross-deployment TCP connect by IP across nodes; cross-deployment UDP; cross-deployment DNS resolution returns NXDOMAIN; cross-network-within-deployment same set.
- **Drift recovery**: operator (test harness) flushes `inet jaco`; within 30s the reconcile loop restores it; an `isolation_ruleset_reconciled` audit event is recorded.
- **Startup failure**: boot a daemon with `nft` unavailable; assert it never reaches ready and other nodes report `isolation_unavailable` for it.

Test rig shape: multi-VM (or rootful systemd-nspawn / docker-in-docker with privileged caps) cluster of 3 nodes, two deployments declared, each with two compose-networks. Each test asserts a specific connectivity outcome via `nc`/`curl` inside a container, plus assertion on `jaco status` and audit log content. Runs in CI on every PR that touches `internal/discovery/`, `internal/runtime/`, or the spec.

## §7 Out of scope

- IPv6 (v1 is IPv4 only; spec doesn't promise IPv6).
- Operator-supplied custom DNS for declared domains (only `<service>` resolution; ingress slice handles external `tls: auto` domain certs but not DNS).
- L7 features (HTTP path-based routing, mTLS sidecars) — those are ingress concerns, not east-west.
- Cross-cluster discovery (spec §3 Out: no federation).
- User-defined network policies beyond compose `networks:` semantics (e.g., NetworkPolicy-style intra-network rules).
- Network performance tuning (MTU adjustment, segmentation offload control) beyond defaults; WG MTU is set at 1420 (standard) without operator override in v1.
- Replacing docker's iptables management — JACO sits on top, does not displace.
- eBPF-based enforcement (faster, more expressive, much harder to operate). Reconsider for v2 if rule cardinality becomes a bottleneck.

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
