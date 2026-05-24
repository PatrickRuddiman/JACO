Parent spec: [Issue #28 — cross-host service discovery](https://github.com/PatrickRuddiman/jaco/issues/28) (driving spec) · repo [spec.md](../../spec.md)

# Cross-host service discovery — control-plane

## §1 Summary
Owns the L3 allocation plan: each `(deployment, network, host)` tuple gets its own `/24` from the IPAM pool so container IPs never collide across hosts. Covers the proto/state key change, leader-authoritative per-host allocation, the lazy per-host trigger in the reconciler, subnet-free cascades on deployment-delete / node-leave, the pre-1.0 migration of host-less subnets, and pool-exhaustion + utilization surfacing. Does not touch how the plan is routed or resolved — those are the datapath and dns slices.

## §2 Codebase reconnaissance
- IPAM allocator at `internal/discovery/ipam/ipam.go` — `Allocate`/`EnsureSubnets`/`nextFree` (lowest-free `/24`, walks third-octets), typed `IPAMError` + `IsExhausted()`. `New(state, applier, poolCIDR)`.
- Subnet store + key at `internal/controlplane/state/subnets.go` — `SubnetKey(deployment, network)`, separator `\x00`.
- Proto entity `Subnet{deployment=1, network=2, cidr=3, allocated_at=4}` at `proto/jaco/v1/entities.proto:203`; commands `SubnetAllocate{deployment=1,network=2,cidr=3}` / `SubnetFree{deployment=1,network=2}` at `proto/jaco/v1/commands.proto:219`.
- FSM at `internal/controlplane/fsm/fsm.go:335` — `fsm.New(state, brokers)` holds **no pool CIDR**; `SubnetAllocate` apply stores the command's `cidr` verbatim (`fsm.go:337`). Apply is the single serialized writer but is **not** a source of the pool.
- `ipam_pool` is **per-node config** (`internal/daemon/config/config.go:49`, must-be-/16, default `10.244.0.0/16`) — not raft state. (Pre-existing bug: `deploy.go:107` passes `ipam.DefaultPoolCIDR`, ignoring the configured value.)
- Current allocation is apply-time on the **leader** at `internal/controlplane/grpc/deploy.go:105` (`ipam.New` + `EnsureSubnets` over `d.raft.Apply`) — leader computes the CIDR and writes it into the command. This is the pattern this slice extends per-host.
- `Internal` gRPC service (`proto/jaco/v1/services.proto:256`, handler `internal/daemon/grpc/internal.go`) is leader-gated: `Submit` returns `Unavailable: no_leader` so callers retry against the leader. New per-host allocation rides a sibling RPC here.
- Reconciler `startReplica` at `internal/runtime/reconciler/reconciler.go:203` reads `state.Subnets.Get(SubnetKey(dep, netSuffix))` before `bridge.Ensure` (line 257); runs on every node, knows `r.hostname`. Its only control-plane write today is `health.SubmitFn = func(ctx, *pb.ReplicaObserved) error` (`health.go:54`) — **ReplicaObserved-only**, cannot carry an allocation command.
- Daemon submit/forward closure at `internal/daemon/grpc/server.go:556` (`node.Apply` on leader, `Internal.Submit` forward via `leaderGRPCAddr` otherwise) — the reusable leader/forward *pattern*, not a generic applier.
- `ReplicaObserved{state, code, message, details, host}` (`entities.proto:127`); `REPLICA_STATE_FAILED=6` (`entities.proto:18`).
- No leadership-change callback wired — only `raft.IsLeader()` polling exists across the codebase.
- No metrics/prometheus dependency — observability is structured log lines. 30s safety-tick is the loop convention (`firewall/reconcile.go:12`, `scheduler.go:44`).

## §3 Decisions
1. **Allocation authority.** Options: FSM computes `nextFree` inside Apply; leader-handler computes (cidr in command); client computes + FSM validates. **Chosen:** leader-handler computes. Rationale: the FSM holds no pool and `ipam_pool` is per-node config, so computing in Apply would diverge raft state across nodes; the leader computing and writing the cidr into the command keeps Apply deterministic, reuses the existing `deploy.go` pattern, and returns the result synchronously.
2. **Command/proto shape.** Options: new `SubnetEnsure` (no cidr); keep `SubnetAllocate` + add `host`. **Chosen:** keep `SubnetAllocate`, add `host`. Rationale: the leader still computes the cidr so it belongs in the command; keeping the message avoids breaking replay of persisted `SubnetAllocate` log entries.
3. **Allocation transport.** Options: inject a generic raft-command submit into the reconciler; a leader-gated `Internal.EnsureSubnet` RPC. **Chosen:** `Internal.EnsureSubnet` RPC. Rationale: a follower can't compute the cidr (stale local state), so it must reach the leader; the leader handler computes + applies and returns the cidr or an exhaustion error in one call — no re-read race.
4. **Migration trigger.** Options: leadership-change callback; boot goroutine polling `IsLeader()`. **Chosen:** boot goroutine. Rationale: no `LeaderCh`/`NotifyCh` is wired — only `IsLeader()` polling exists.
5. **Pool-exhaustion surfacing.** Options: synchronous RPC error → reconciler marks FAILED; FSM-side rejection. **Chosen:** the `EnsureSubnet` RPC returns a typed exhausted error; the reconciler emits `ReplicaObserved{FAILED, subnet_pool_exhausted}` via the existing `SubmitFn` and skips `bridge.Ensure`. Rationale: synchronous and unambiguous (no replication-lag-vs-exhaustion confusion).
6. **Utilization log placement.** Options: leader `EnsureSubnet` handler post-alloc; reconciler post-ensure; periodic ticker. **Chosen:** leader handler, event-driven after a successful new alloc. Rationale: single place, leader-only (no N-node duplicate lines), fires exactly when utilization changes, no replay spam.
7. **Leader-failover handling in the reconciler.** Options: distinguish `no_leader` from `subnet_pool_exhausted` and retry the former on the tick; inline bounded retry; treat any error as FAILED. **Chosen:** distinguish + retry on tick. Rationale: a routine election must not permanently fail a replica — only true exhaustion is terminal; the reconciler already re-runs on watch events and the 30s safety tick.

## §4 Contracts & shapes
**Proto (`proto/jaco/v1`)**
- `Subnet` (entities.proto): add `host string` = field 5. Other fields unchanged.
- `SubnetAllocate` (commands.proto): add `host string` = field 4. `cidr` (field 3) retained — the leader fills it.
- `SubnetFree` (commands.proto): add `host string` = field 3. Empty `host` ⇒ free every host's subnet for `(deployment, network)`; empty `network` + set `host` ⇒ free every subnet on `host` (node-leave cascade).
- `Internal` service (services.proto): add `rpc EnsureSubnet(EnsureSubnetRequest) returns (EnsureSubnetResponse)`. Request `{deployment, network, host}`. Response `{cidr}`. Pool-exhaustion returns a gRPC error carrying code `subnet_pool_exhausted`.

**State key (`internal/controlplane/state/subnets.go`)**
- Store key becomes `deployment \x00 network \x00 host`. `SubnetKey` signature → `SubnetKey(deployment, network, host string)`; all callers updated (no 2-arg form survives).

**IPAM allocator (`internal/discovery/ipam/ipam.go`)**
- `Allocate(deployment, network, host string) (*pb.Subnet, error)` — guarded by an allocator mutex so concurrent leader-side calls serialize. Idempotent: returns the existing Subnet when `(deployment, network, host)` is on file; else computes `nextFree` over current `state.Subnets`, raft-applies `SubnetAllocate{deployment, network, host, cidr}`, returns it. Pool comes from `config.IPAMPool` (fixes the `DefaultPoolCIDR` bug). `nextFree` stays here (not moved to the FSM); `/16`-only third-octet walk unchanged.
- Remove `EnsureSubnets`. `IsExhausted` retained for the RPC error path.

**Leader RPC handler (`internal/daemon/grpc/internal.go` — `EnsureSubnet`)**
- Leader-gated (mirrors `Submit`: returns `Unavailable: no_leader` when not leader). Calls `ipam.Allocate(deployment, network, host)`; on success logs utilization (`len(state.Subnets) / pool_/24_count`; WARN ≥75%, ERROR ≥90%, tuple named) only when a new subnet was written; on `IsExhausted` returns the `subnet_pool_exhausted` gRPC error.

**Reconciler trigger (`internal/runtime/reconciler/reconciler.go`)**
- `reconciler.New` gains an injected `ensureSubnet func(ctx, deployment, network, host string) (cidr string, err error)`. The daemon wires it to call `ipam.Allocate` directly when self is leader, else forward to the leader's `Internal.EnsureSubnet` (reusing `leaderGRPCAddr` from `server.go`).
- `startReplica`: for each declared network, before `bridge.Ensure`, call `ensureSubnet(ctx, rep.Deployment, netSuffix, r.hostname)`. Use the returned `cidr` for `bridge.Ensure` (no state re-read). Error handling distinguishes the two failure classes:
  - `subnet_pool_exhausted` (terminal) → emit `ReplicaObserved{id, state=REPLICA_STATE_FAILED, code:"subnet_pool_exhausted", details:{deployment, network, host}}` via `submit` and return without starting the container.
  - `no_leader` / `Unavailable` (transient — election in progress) → log and return without emitting FAILED; the replica is retried on the next `ReplicasDesired` watch event or the 30s safety tick. `ensureSubnet` must surface these as distinct error classes so the reconciler can tell them apart.
- Subnet reads switch to the 3-arg key with `host=r.hostname` (`startReplica` line 253, `resolveDNSServers` at `reconciler.go:42`).

**FSM apply (`internal/controlplane/fsm/fsm.go`)**
- `SubnetAllocate`: write `Subnet{deployment, network, host, cidr, allocated_at=cmd.Ts}` (host added). No compute — value comes from the command (stays deterministic).
- `SubnetFree`: remove by the most-specific key present; cascade-remove all matching when `host` and/or `network` are empty.
- `DeploymentDelete` cascade (`fsm.go:180`): add a pass removing every `Subnet` whose `deployment == name`. Closes the existing leak.
- `NodeRemove` cascade (`fsm.go:104`): add a pass removing every `Subnet` whose `host == hostname`.

**Boot-time migration (daemon startup)**
- A boot goroutine polls `raft.IsLeader()` until this node is leader and the FSM has caught up, then applies `SubnetFree` for every `Subnet` with empty `host`, once (guarded so it doesn't repeat on re-election). Logged.

**Apply-time path removal (`internal/controlplane/grpc/deploy.go`)**
- Remove the `ipam.New` + `EnsureSubnets` block (`deploy.go:100-114`). Apply no longer allocates subnets; placement → reconciler `ensureSubnet` is the sole trigger.

## §5 Sequence
1. Operator `jaco apply` → `deploy.go` writes the Deployment (no subnet allocation). → scheduler places replicas, writing `ReplicaDesired{host=H}`.
2. On host H the reconciler's watch fires → `startReplica` runs for `host=self`. → for each network it calls `ensureSubnet(dep, net, H)`.
3. `ensureSubnet` reaches the leader (local call if H is leader, else `Internal.EnsureSubnet` forward). → leader's handler calls `ipam.Allocate` under the allocator mutex: existing tuple → return its cidr; else `nextFree` over `state.Subnets` → raft-apply `SubnetAllocate{dep,net,H,cidr}` → return cidr. → handler logs utilization if it crossed a threshold.
4. Reconciler receives the cidr → `bridge.Ensure(..., cidr, ...)` (datapath slice). On `subnet_pool_exhausted` → emit `ReplicaObserved{FAILED, subnet_pool_exhausted}` and stop → operator sees the failed replica with the named tuple.
5. Operator `jaco delete <dep>` → `DeploymentDelete` cascade removes all `Subnet{deployment==dep}` → freed `/24`s return to the pool.
6. Operator `NodeRemove H` → `NodeRemove` cascade removes all `Subnet{host==H}` → freed `/24`s return to the pool.
7. Daemon upgrade → the new leader's boot goroutine applies `SubnetFree` for every host-less `Subnet` → reconcilers re-ensure per-host `/24`s on the next tick.

## §6 Out of scope
- Bridge creation, kernel routes via `jaco0`, MTU, route reconciliation, WG AllowedIPs, nftables → **datapath** slice.
- DNS Manager bind ordering, cluster-wide answers, locality preference, `NetworkConnect` aliases → **dns** slice.
- Relaxing the `/16` pool constraint to `/15`–`/14` (the issue's large-cluster mitigation). `config.go:134` and `nextFree`'s third-octet-only walk both hard-assume `/16`; supporting wider masks is a separate change.
- Moving `ipam_pool` into cluster (raft) state. The leader's config governs allocation (as it does today); per-node config divergence is moot because only the leader computes.

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
