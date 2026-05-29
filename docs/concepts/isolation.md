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

## The no-host-disruption invariant

JACO's ruleset is **all-accept by policy**. Every base chain is
`policy accept`; JACO never blanket-drops host ingress or forwarded
traffic it does not own. The operator's other docker networks, host
routing, SSH (on whatever port), a VPN, a VNet, their own host
firewall — all of that is the operator's domain, and a policy-drop
chain would silently trespass on choices JACO cannot know about.

The **only** packets JACO drops are those flowing **between two of its
own container subnets that belong to different (deployment, network)
scopes**. This is scoped to a `jaco_pool` set (the union of every JACO
subnet), so anything outside JACO's own address space is never matched.

Source of truth: [`render.go`](../../internal/discovery/firewall/render.go)
(`Render` is pure-Go and golden-file tested).

## The ruleset

JACO manages a single nftables table, `inet jaco`. With one workload
deployment `front` on the default network, the rendered table looks
like:

```
table inet jaco {
    set dep_net_front__default {
        type ipv4_addr
        flags interval
        elements = { 10.244.1.0/24 }
    }
    set jaco_pool {
        type ipv4_addr
        flags interval
        elements = { 10.244.1.0/24 }
    }

    chain forward {
        type filter hook forward priority 0; policy accept;
        ct state established,related accept
        ip saddr @dep_net_front__default ip daddr @dep_net_front__default accept
        ip saddr @jaco_pool ip daddr @jaco_pool drop
    }

    chain input {
        type filter hook input priority 0; policy accept;
    }

    chain output {
        type filter hook output priority 0; policy accept;
    }
}
```

### Named sets

- One `set dep_net_<dep>_<net>` per (deployment, network), holding
  **every host's** `/24` for that scope (per-host /24s, issue #28) so
  cross-host same-scope traffic matches `@set` on both saddr and
  daddr. Names are sanitized to `[a-zA-Z0-9_]` and hashed when they
  would exceed nftables' 63-char identifier limit (`SetName`).
- `set jaco_pool` — the union of every JACO subnet. Emitted **only
  when at least one subnet exists**. It scopes the cross-network drop.

### `chain forward`

`type filter hook forward priority 0; policy accept;`

Rules in order:

1. `ct state established,related accept` — return path for
   already-allowed flows.
2. Per (deployment, network), one rule:
   `ip saddr @<set> ip daddr @<set> accept` — same-(deployment,
   network) traffic, anywhere in the cluster, regardless of whether
   the inbound interface is a JACO bridge or `wg-jaco`.
3. `ip saddr @jaco_pool ip daddr @jaco_pool drop` — the cross-scope
   isolation drop, emitted only when subnets exist. Two JACO containers
   in different scopes both fall in `jaco_pool` but match no per-set
   accept, so this rule fires. Anything where either address is outside
   `jaco_pool` falls through to the **accept** policy untouched.

### `chain input`

`type filter hook input priority 0; policy accept;`

**No rules.** JACO does not police host ingress. WireGuard
(`udp/51820`), the gRPC API (`tcp/7000`), the per-bridge DNS responder
(`udp/53`), and the public Caddy ports (`tcp/80,443`) are all reachable
because the chain accepts by default — JACO does not add explicit
allows for them. The chain exists only so the table's shape is stable
for drift detection.

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
of scope. JACO's rules live in a **separate** nftables table; the
scoped `jaco_pool` cross-set drop composes with docker's parallel
chains because any drop from any chain stops the packet.

Because JACO's chains are all-accept, cross-host container traffic must
also survive docker's *own* isolation drops. The firewall reconciler
re-asserts two exemptions in docker-owned chains on every tick (issue
#28), gated on the IPAM pool being known:

- **`EnsureSNAT`** — an intra-pool SNAT exemption in docker's nat
  `POSTROUTING` so pool-to-pool traffic keeps its real source address.
  A failure audits `SNAT_EXEMPT_FAILED`.
- **`EnsureOverlay`** — intra-pool ACCEPT exemptions (raw `PREROUTING`
  direct-routing + `DOCKER-USER` inter-network isolation) so
  WG-decrypted cross-host packets aren't dropped by docker's
  container-isolation rules. A failure audits `OVERLAY_EXEMPT_FAILED`.

These live outside `table inet jaco` (and outside its self-test), so
they are re-checked best-effort every 30 s independently of the
isolation status.

## Atomic reload

JACO renders the **full expected ruleset** for the node on every
relevant watch event (Subnets, ReplicaObserved, Nodes), debounced at
200 ms, and submits the whole thing as one transaction via `nft -f`.
Partial state is impossible — either the new ruleset is in place or
the old one still is.

## Self-test on startup

After first load, JACO reads back `nft -j list table inet jaco` and
checks it against the rendered expectation (`SelfTestFromJSON`): the
three base chains are present with `policy accept`, one
`dep_net_<dep>_<net>` set exists per scope, and `jaco_pool` exists iff
there is at least one subnet. On mismatch:

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
- By guessed IP — packet enters FORWARD; no per-scope set contains both
  `10.244.1.x` and `10.244.7.x`, so no per-set ACCEPT matches; both
  addresses are in `jaco_pool`, so the scoped cross-set `drop` fires.
  Same on cross-node attempts.

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
