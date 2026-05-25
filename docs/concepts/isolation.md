# Isolation

JACO enforces two isolation boundaries cluster-wide:

1. **Cross-deployment** — containers in different deployments cannot
   reach each other. Period.
2. **Cross-network within a deployment** — containers in the same
   deployment but on disjoint compose `networks:` cannot reach each
   other.

Both are enforced by **L2 bridge separation** (one bridge per
(deployment, network); see [Networking](networking.md)) **and** L3
**nftables** rules in the `inet jaco` table. Both layers must be
operational on every node before that node is considered ready.

Code: [`internal/discovery/firewall/`](../../internal/discovery/firewall).
Design: [`slices/discovery.md`](../planning/slices/discovery.md) §4.

## The ruleset

JACO manages a single nftables table, `inet jaco`, with three chains:

### `chain forward`

`type filter hook forward priority 0; policy drop;`

Rules in order:

1. `ct state established,related accept;` — return path for
   already-allowed flows.
2. Per (deployment, network), one rule:
   `ip saddr @<set_X> ip daddr @<set_X> accept;` — same-(deployment,
   network) traffic, anywhere in the cluster, regardless of whether
   `iifname` is a JACO bridge or `wg-jaco`. The set names follow
   `dep_net_<dep>_<net>` with sanitization for long names.
3. Implicit DROP (the chain policy).

### `chain input`

`type filter hook input priority 0; policy drop;`

Rules in order:

1. `iifname "lo" accept;`
2. `udp dport 51820 accept;` — WireGuard ingress.
3. `iifname "wg-jaco" accept;` — already filtered upstream.
4. JACO-bridge → DNS responder on port 53.
5. `tcp dport 7000 accept;` — JACO gRPC API.
6. `tcp dport {80, 443} accept;` — public Caddy ingress.
7. Implicit DROP.

### `chain output`

`type filter hook output priority 0; policy accept;`

JACO does not constrain egress from the host itself.

## Why IP sets (not interface matching)

Cross-node traffic arrives via `wg-jaco`, not a JACO bridge — `iifname`
matching alone would miss it. IP-set matching, keyed on
`(saddr, daddr)` both being members of the same
`dep_net_<dep>_<net>` set, works uniformly for same-node bridge-to-bridge
and for cross-node WG-decrypted paths.

## Coexistence with docker

JACO leaves docker's iptables management enabled — it handles NAT for
outbound container traffic and per-bridge defaults; replacing it is out
of scope. JACO's rules live in a **separate** nftables table whose
DROP outcomes apply regardless of docker's parallel chains. Any DROP
from any chain stops the packet, so the two layers compose correctly.

## Atomic reload

JACO renders the **full expected ruleset** for the node on every
relevant watch event (Subnets, ReplicaObserved, Nodes), debounced at
200 ms, and submits the whole thing as one transaction via `nft -f`.
Partial state is impossible — either the new ruleset is in place or
the old one still is.

## Self-test on startup

After first load, JACO runs a synthetic check via `nft -n list
ruleset`: a same-(deployment, network) ACCEPT path and a
cross-deployment DROP path. On mismatch:

- `Error{code: isolation_self_test_failed}` is logged + audited.
- The daemon does NOT call `sd_notify(READY=1)` — systemd holds the
  unit in `starting`.
- `Node.status` in raft stays `joining`, then transitions to
  `isolation_unavailable` once admission opens; other nodes see this
  and skip the host for scheduling.

Operator action: confirm the kernel has nftables, the `nft` binary is
on PATH, and the daemon has `CAP_NET_ADMIN`. Then `systemctl restart
jacod` to retry.

## Drift reconcile

A safety tick runs every 30 s:

1. `nft list table inet jaco` and diff against the expected rendered
   ruleset.
2. On any drift (operator manually edited, another tool clobbered the
   table, transient kernel issue): re-render, `nft -f` atomic reload,
   emit an `AuditEvent{type: isolation_ruleset_reconciled, identity:
   system, payload: {node, diff}}`.
3. If reload fails: transition the node to `isolation_unavailable`;
   cease accepting new container creates. **Existing containers
   continue running** — JACO does not amplify damage by tearing them
   down on drift recovery failure.

The isolation rig (`scripts/test/isolation-rig.sh`) exercises this in
CI: it flushes the table out-of-band and asserts the reconcile
restores it within 30 s plus the audit event is recorded.

## What containers can and cannot do

A container in deployment `front` (subnet `10.244.1.0/24`) attempting
to reach a container in deployment `back` (subnet `10.244.7.0/24`):

- DNS — `back.some-service` returns NXDOMAIN immediately (the
  responder on `front`'s bridge only knows `front`'s services).
- By guessed IP — packet enters FORWARD; no named set contains both
  `10.244.1.x` and `10.244.7.x`; the implicit DROP fires. Same on
  cross-node attempts.

Multi-network within one deployment behaves identically: the named
sets are per-(deployment, network), not per-deployment, so a `frontend`
container cannot reach a `backend`-only service even within the same
deployment unless a bridge service (declared on both networks)
relays.

## Practical implications

- **Operator hygiene** — do not hand-edit `inet jaco`. JACO will
  reconcile within 30 s, emit an audit event, and you will have to
  explain it.
- **CI / dev clusters** — the rig requires CAP_NET_ADMIN, CAP_NET_RAW,
  kernel WG, nftables, and docker. `make test-isolation` runs the rig;
  `make ci-test` skips it. See
  [Testing](../contributing/testing.md).
- **Production** — every node MUST satisfy the kernel gates. A
  partially gated cluster is supported (the affected node sits in
  `isolation_unavailable`, others schedule normally), but operators
  should fix the gated node before treating the cluster as
  production-healthy.

## See also

- [Networking](networking.md)
- [Status and errors](status-and-errors.md) — the
  `isolation_unavailable` and `isolation_ruleset_reconciled` codes
- [Troubleshooting](../operations/troubleshooting.md)
- [`slices/discovery.md`](../planning/slices/discovery.md)
