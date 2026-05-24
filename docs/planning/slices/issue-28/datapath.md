Parent spec: [Issue #28 — cross-host service discovery](https://github.com/PatrickRuddiman/jaco/issues/28) (driving spec) · repo [spec.md](../../spec.md)

# Cross-host service discovery — datapath

## §1 Summary
Makes the per-host `/24` allocation plan (see [control-plane](control-plane.md)) actually carry packets: each peer's WireGuard `AllowedIPs` gains that peer's container subnets, the host installs kernel routes for every other host's `/24` via `jaco0`, the per-host bridge is created at MTU 1420, and the nftables east-west filter accepts cross-host intra-`(deployment,network)` traffic. Does not allocate subnets, resolve names, or set container aliases — those are the control-plane and dns slices.

## §2 Codebase reconnaissance
- Bridge create at `internal/discovery/bridge/bridge.go` — `Ensure(ctx,d,deployment,network,cidr,clusterID)` sets `IPAM.Config{Subnet,Gateway}` + `com.docker.network.bridge.name`; **no MTU option**. `GatewayIP(cidr)` → `.1`.
- WG sync at `internal/discovery/wgmesh/sync.go` — `Syncer.Run` reconciles `state.Nodes` onto the kernel device every 30s (`DefaultSyncInterval`); `BuildConfig` (sync.go:147) sets each peer `AllowedIPs = [peer /32]` with `ReplaceAllowedIPs:true`. `EnsureInterface` shells out to `ip link add` (os/exec). Syncer wired at `internal/daemon/grpc/server.go:419`.
- No `vishvananda/netlink` in `go.mod` — only `github.com/mdlayher/netlink` (low-level, indirect). Existing route/link manipulation shells out to `ip`.
- Firewall ruleset at `internal/discovery/firewall/render.go` — `table inet jaco`, filter chains only (`forward` policy drop, `ct established,related accept`, one `ip saddr @<set> ip daddr @<set> accept` per Subnet). Set keyed by `SetName(dep,net)`, `elements = { single CIDR }`. **No NAT chain.**
- Firewall RuleInput built cluster-wide at `internal/daemon/grpc/server.go:477` (iterates all `st.Subnets.List()`); selftest keys `wantSets` by `SetName` (`internal/discovery/firewall/selftest.go:87`); reconcile drift loop at `internal/discovery/firewall/reconcile.go`.
- Docker installs its own per-bridge MASQUERADE in the iptables-compat `ip nat POSTROUTING` chain — outside `table inet jaco`.
- Pool CIDR is config-driven: `internal/daemon/config/config.go:48` (`ipam_pool`, default `10.244.0.0/16`).

## §3 Decisions
1. **Reconciler topology for AllowedIPs + kernel routes.** Options: extend `wgmesh.Syncer`; separate `route_reconciler.go` goroutine. **Chosen:** extend `Syncer`. Rationale: `AllowedIPs` already lives in the Syncer's `ConfigureDevice`, and routes derive from the same `state.Nodes`+`state.Subnets` — one tick avoids coordinating two loops over the same state.
2. **Kernel-route mechanism.** Options: shell out to `ip`; add `vishvananda/netlink`. **Chosen:** shell out to `ip`, parsing **plain-text** `ip route show dev jaco0` (not `-j`/JSON). Rationale: matches the existing `ip link add` in `EnsureInterface`, no new dependency, and plain-text output is universal — the package targets apk (Alpine), whose busybox `ip` lacks `-j`.
3. **Firewall set grouping for per-host CIDRs.** Options: `Render` aggregates by `SetName`; change `firewall.Subnet` to carry `CIDRs []string`. **Chosen:** `Render` aggregates. Rationale: smallest blast radius — `firewall.Subnet` and the selftest (keyed by `SetName`) are untouched.
4. **Docker MASQUERADE / SNAT exemption.** Options: audit-first then a RETURN rule; nat chain in `table inet jaco`; assume-present. **Chosen:** audit-first, then a RETURN rule in Docker's own `nat POSTROUTING`, **reconciled on the firewall 30s tick**. Rationale: a separate `inet jaco` nat chain wouldn't bypass Docker's `ip nat` table at the same hook; Docker rewrites `nat` on every network create/restart, so the rule must be re-asserted on a tick or it gets buried below the masquerade.
5. **Bridge MTU (forced).** Hardcode 1420 via `com.docker.network.driver.mtu`. Rationale: fixed WG overhead; no caller needs it configurable (anti-slop: no speculative knob).

## §4 Contracts & shapes
**Bridge (`internal/discovery/bridge/bridge.go`)**
- `Ensure` adds `Options["com.docker.network.driver.mtu"] = "1420"` (new package constant `BridgeMTU = 1420`). Signature and existing behavior otherwise unchanged; the per-host `cidr` is already a parameter (supplied by the control-plane reconciler).

**WG AllowedIPs (`internal/discovery/wgmesh/sync.go` — `BuildConfig`)**
- For each peer (`state.Nodes` where hostname != self), `AllowedIPs` becomes `[peer /32] + [every Subnet.CIDR where Subnet.host == peer.hostname]`. `ReplaceAllowedIPs:true` retained so freed subnets drop out. Disjointness across peers holds because per-host allocation guarantees no `/24` collision; self's own subnets are never added to a peer (they route over the local bridge).

**Kernel routes (`internal/discovery/wgmesh/sync.go` — new step in `Syncer.tick`)**
- Desired set: `{ Subnet.CIDR for every (deployment,network,host) in state.Subnets where host != SelfHostname }`.
- Current set: parse plain-text `ip route show dev jaco0` → the installed `/24`s (one CIDR per line; busybox-compatible, no JSON).
- Diff → `ip route add <cidr> dev jaco0` for missing, `ip route del <cidr> dev jaco0` for orphans. Idempotent each tick.
- Runs after `ConfigureDevice` in the same tick; guarded on `jaco0` existing (skip + log-once on absence, mirroring the existing `loggedConfigError` pattern). Routes for self's own subnets are never installed (they stay on the local bridge).

**Firewall (`internal/discovery/firewall/render.go` — `Render`)**
- Group `in.Subnets` by `SetName(deployment,network)`. Emit one `set` per group with `elements = { cidr1, cidr2, ... }` (all per-host CIDRs of that `(deployment,network)`, sorted for determinism), and one `ip saddr @<set> ip daddr @<set> accept` per group in the `forward` chain. Cross-host packets (saddr in host-A's `/24`, daddr in host-B's `/24`) now match because both `/24`s are in the same set. `firewall.Subnet{Deployment,Network,CIDR}` and `selftest.go` unchanged.

**SNAT exemption (gated on audit)**
- Audit step: on the 3-node repro / CI rig (`tasks/31-isolation-ci-test-rig.md`), confirm whether Docker's per-bridge MASQUERADE rewrites the source of a `jaco0`-routed container→container packet.
- If confirmed: assert a RETURN at the top of Docker's `nat POSTROUTING` chain matching `ip saddr <ipam_pool> ip daddr <ipam_pool>` (pool from `config.IPAMPool`, not hardcoded). This lives outside `table inet jaco`, so the firewall `Reconciler` (`internal/discovery/firewall/reconcile.go`) gains a second, independent check on its existing 30s tick: confirm the RETURN sits above Docker's masquerade in `nat POSTROUTING` and re-insert it if missing/buried. Tracked separately from the `inet jaco` SelfTest verdict.

## §5 Sequence
1. control-plane reconciler resolves the per-host `/24` for `(dep,net,self)` → calls `bridge.Ensure(...,cidr,...)` → docker network created at MTU 1420 with gateway `.1`.
2. `Syncer.tick` (30s / on next cycle): `BuildConfig` reads `state.Nodes`+`state.Subnets` → `ConfigureDevice` sets each peer's `AllowedIPs` = peer `/32` + that peer's container `/24`s.
3. Same tick: route step parses `ip -j route show dev jaco0`, diffs against desired (`state.Subnets`, host != self), runs `ip route add/del ... dev jaco0`.
4. Container on host A sends to host B's container IP → `forward` chain matches `saddr@set daddr@set` (set now holds both hosts' `/24`s) → accept → kernel route sends it to `jaco0` → WG `AllowedIPs` matches B's `/24` → encrypted to B.
5. On B: packet arrives on `jaco0` → `forward` chain accepts (same set membership) → routed to B's local bridge → delivered.
6. Node leave / deployment delete frees subnets (control-plane FSM cascade) → next `Syncer.tick` prunes the peer's `AllowedIPs` and `ip route del`s the orphaned `/24`; next firewall reconcile drops the set element.
7. (Gated) audit confirms Docker masquerade fires → RETURN rule inserted in `nat POSTROUTING` so cross-host source IPs survive.

## §6 Out of scope
- Subnet allocation, the `host` proto/state key, pool exhaustion, utilization logging → **control-plane** slice.
- DNS Manager bind ordering, cluster-wide answers, locality preference, `NetworkConnect` aliases → **dns** slice.
- Relaxing the `/16` pool mask. Routes/AllowedIPs derive from whatever CIDRs `state.Subnets` holds, so they're mask-agnostic, but pool sizing is decided in control-plane.
- WG keypair/peer-endpoint management and `EnsureInterface` (already shipped; unchanged except the AllowedIPs/route additions).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
