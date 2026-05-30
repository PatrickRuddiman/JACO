---
sources:
  - internal/discovery/firewall/
  - internal/daemon/grpc/apply_or_forward.go
  - internal/daemon/grpc/server.go:applyOrForward
  - internal/runtime/compose/validate.go:allowedServiceFields
---

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
deployment `front` on the default network, the rendered ruleset looks
like:

```
add table inet jaco
delete table inet jaco
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

The leading `add table inet jaco` / `delete table inet jaco` pair is the
atomic-replace prelude (see [Atomic reload](#atomic-reload)): `add`
creates the table if it's absent so the following `delete` can't fail on
a cold host, and `delete` drops the entire prior generation so a
re-apply rebuilds the table from scratch instead of appending to it.

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

The rendered file leads with `add table inet jaco` then
`delete table inet jaco` before the `table inet jaco { … }` body. This
matters because `nft -f` **appends** to an existing chain rather than
replacing it: re-applying the table body on every reconcile would stack
a fresh generation of forward rules onto the live chain each time. An
earlier generation's `@jaco_pool … drop` then sits *ahead* of a
later-deployed stack's per-scope `accept`, shadowing it — silently
breaking cross-host traffic for every deployment except the first
applied (same-host traffic L2-switches within one bridge and never hits
the forward hook, which is what made the bug subtle; issue #89).
Deleting the table first means each apply recreates it from scratch
within the one `nft -f` transaction, so the chains never accumulate. The
`add` ahead of the `delete` keeps the `delete` from failing on a cold
host where the table doesn't exist yet. The SNAT/overlay exemptions live
in Docker's own `nat`/`raw` tables (re-asserted each tick), so flushing
`inet jaco` doesn't disturb them.

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

### Leader-forwarded audit and status (issue #88, #112, #113)

The `Audit` and `UpdateStatus` callbacks both write to raft. Direct
`node.Apply` only succeeds on the leader; on a follower it returns
`hraft.ErrNotLeader`. The reconciler routes both callbacks through the
`applyOrForwardCommand` shim:

- on the leader → direct `node.Apply`;
- on a follower → dial the leader's gRPC address (resolved from
  `state.Nodes`) and call `Internal.Submit` to apply the same command
  cluster-wide.

Before this shim, every follower's reconcile audited
`ISOLATION_RULESET_RECONCILED` and called `UpdateStatus`, both failed
with `ErrNotLeader`, and the reconciler logged
`Audit(...) failed` + `firewall.Reconciler.Tick failed` even though
the underlying `nft -f` apply had succeeded. The audit event was also
lost.

A freshly-joined follower's first tick can still race ahead of raft
leader discovery — at that point `state.Nodes` carries no leader gRPC
address and the forward fails. To suppress the spurious startup-window
errors, `Reconciler.ReadyGate` is wired to `node.Leader() != ""`. While
the gate returns `false`, `Loop` skips `Tick` and waits for the next
ticker. Steady-state behavior is unchanged once raft settles.

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

## Compose namespace knobs weaken isolation

Compose accepts a closed allowlist of namespace knobs that JACO
forwards verbatim into docker's `HostConfig` (issue #118): `ipc`,
`pid`, `uts`, `userns_mode`, `cgroup`, `cgroup_parent`. Host-mode
values (`pid: host`, `ipc: host`, `uts: host`, `userns_mode: host`)
share the host kernel's namespace with the container and weaken
isolation by design. JACO does **not** gate them at apply time — an
operator declaring `pid: host` is presumed to know they are giving the
container visibility into every host process. Operator policy (e.g.
rejecting host-mode at admission) is a separate decision tracked
outside this iteration.

The bridge / nftables isolation described above still holds: a
container with `pid: host` sees the host's processes but its network
traffic is still subject to the per-scope set match in `chain forward`.

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
- **Host-kernel surface** — compose `devices:` (issue #115),
  `privileged:`, and `host`-mode namespace knobs (`pid: host`,
  `ipc: host`, `uts: host`, `userns_mode: host`) all weaken
  isolation by design. JACO honors them as-written today; an
  operator-side policy gate (label/selector based) is on the
  roadmap so deployments that need raw host access can be opted
  in explicitly per node, without forcing every workload through
  the same surface.

## See also

- [Networking](networking.md)
- [Status and errors](status-and-errors.md) — the
  `isolation_unavailable` and `isolation_ruleset_reconciled` codes
- [Troubleshooting](../operations/troubleshooting.md)
