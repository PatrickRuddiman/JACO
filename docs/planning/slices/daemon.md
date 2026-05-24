Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — daemon

## §1 Summary

Single long-running `jacod` binary that, once started, hosts every per-host
runtime: raft node, gRPC server (both unix-socket + TLS over TCP), scheduler
reconcile + rollout + restart + drain loops on the leader, runtime
lifecycle / health-watcher / log-streamer per replica, discovery WireGuard
mesh + nftables ruleset + per-bridge DNS, ingress Caddy + ACME reload loop.

`jacod` does not own any operator-driven workflow. It boots, listens, and
serves. Cluster bring-up is operator-driven through the `jaco` CLI which
RPCs into the local jacod via the unix socket. The CLI never reaches into
`$JACO_DATA_DIR` directly.

## §2 Codebase reconnaissance

This slice is the cross-cutting integrator. Every other slice already ships
its primitives (`Run(ctx)` / `Start(ctx)` / `Reconcile(ctx)` entry points)
with unit tests. The work here is goroutine orchestration + lifecycle +
config loading, not new business logic.

The `jaco bootstrap` cobra command (task 05) and the bootstrap library
(`internal/controlplane/bootstrap/`) currently live on the CLI side. They
move: the library stays where it is and gets invoked from the daemon-side
RPC handler; the cobra command moves under `cmd/jaco/cluster.go` as a thin
gRPC client.

## §3 Decisions

1. **Two binaries, not one.** Options: single binary with `jaco serve`
   subcommand; separate `jaco` (CLI) + `jacod` (daemon). **Chosen:**
   separate. Rationale: matches docker / etcd / consul operator
   expectations; makes the daemon's resource needs (CAP_NET_ADMIN, the jaco
   system user) cleanly separable from CLI which runs unprivileged;
   `self-upgrade` swaps both atomically (one extra rename, no semantic
   change).

2. **Daemon startup is passive, not init-driven.** Options: `jacod
   --bootstrap-cluster` flag; auto-detect raft state and refuse to start
   without it; passive listen + reject most RPCs until `Cluster.Init`.
   **Chosen:** passive listen. Rationale: lowest-friction operator UX —
   `systemctl start jacod` always works; cluster bring-up is its own
   step driven by `jaco`. systemd unit has no JACO-specific flags.

3. **Local control via unix socket.** Options: localhost TLS only; unix
   socket + TLS for cross-host. **Chosen:** unix socket
   (`/var/run/jaco/jaco.sock`, mode 0660, owner root, group jaco) for local
   control + TLS-over-TCP for cross-host. Rationale: lets the CLI talk to
   its local daemon before any cluster CA exists; matches docker.sock
   conventions; the unix socket is the trust boundary (anyone in the jaco
   group can call init/join).

4. **Cluster init + join are RPCs, not CLI subcommands.** Options: CLI
   does raft work directly (today); CLI RPCs daemon. **Chosen:** RPCs. New
   `Cluster.Init(InitRequest) returns (InitResponse{cluster_id,
   operator_token})` and `Cluster.Join(JoinRequest{peer_addr, join_token})
   returns (JoinResponse{})`. Rationale: daemon owns `$JACO_DATA_DIR/`;
   CLI never touches raft state directly; works the same on a fresh node
   as on a node that already has raft state (both go through the daemon's
   admission path).

5. **Pre-init RPC admission.** Options: separate "uninitialized" listener;
   single listener with per-method gating. **Chosen:** single listener,
   per-method gating. Until raft state exists on disk, only
   `Cluster.{Init, Join, Status}` accept; everything else returns
   `cluster_uninitialized`. After Init or Join completes, full admission
   activates and the gate flips. Rationale: simpler wire surface; the
   `cluster_uninitialized` code is what `jaco status` on a fresh node
   should report anyway.

6. **Daemon config lives in `jacod.yaml`.** Options: flags only; env-only;
   config file. **Chosen:** YAML config file at
   `/etc/jaco/jacod.yaml`, loaded at startup. Schema is the closed set
   needed for steady-state operation (data_dir, listen_addr, wg_port,
   acme_email, log_level). `jaco cluster init` does not need to ship
   any of these — they're already loaded in jacod before the CLI shows up.

7. **Goroutine lifecycle.** Options: one big context-with-cancel; per-
   subsystem context tree. **Chosen:** per-subsystem context tree with one
   root from main. Root cancellation tears everything down in reverse
   start order via `sync.WaitGroup`. SIGTERM → root cancel; SIGINT → same.
   No graceful-vs-hard distinction; raft writes are synchronous so
   in-flight Apply calls complete before the FSM closes.

## §4 Contracts & shapes

`cmd/jacod/main.go` — entry point. Roughly:

```
main()
  load jacod.yaml
  open unix socket + TLS TCP listener (TLS cert from $DATA_DIR/node/ if exists)
  start gRPC server with admission interceptor that knows the
    initialized-vs-uninitialized state
  if $DATA_DIR/raft/ is non-empty:
    open raft node
    spawn scheduler.Run, restart.Run, ingress.Rebuild.Run,
      runtime.Health.Run, firewall.Reconciler.Loop, wgmesh.Sync,
      etc. via the goroutine tree
  block on ctx (SIGTERM / SIGINT cancels)
```

`cmd/jaco/cluster.go` — new CLI subcommands:

- `jaco cluster init [--cluster-name <n>]` dials the local unix socket,
  calls `Cluster.Init`, prints the returned operator token. Idempotent
  refuse: returns `cluster_already_initialized` if raft state already
  exists.
- `jaco cluster status` — convenience wrapper around `Cluster.Status` (the
  existing RPC) that prints whether the local node is initialized + its
  raft role.

`cmd/jaco/node.go` join (replaces today's direct raft dial):

- `jaco node join --peer host:7000 --token <single-use>` dials the local
  unix socket, calls `Cluster.Join(peer_addr, join_token)`. Daemon owns
  the cross-host raft dial.

Proto additions in `proto/jaco/v1/services.proto`:

```
service Cluster {
  ...
  rpc Init(InitRequest) returns (InitResponse);
  rpc Join(JoinRequest) returns (JoinResponse);
}

message InitRequest {
  string cluster_name = 1; // optional; defaults to uuid
}
message InitResponse {
  string cluster_id = 1;
  string operator_token = 2; // shown once, never recoverable
}
message JoinRequest {
  string peer_addr = 1;
  string join_token = 2;
}
message JoinResponse {}
```

Daemon-side admission gate:

- A new `admission.InitGate` interceptor wraps the existing token-based
  interceptor. Before each RPC, it checks the daemon's `Initialized`
  flag. If false, only `/jaco.v1.Cluster/Init`,
  `/jaco.v1.Cluster/Join`, and `/jaco.v1.Cluster/Status` are allowed
  (no token required on the unix socket); everything else returns
  `Unavailable` + `cluster_uninitialized`.
- Once raft state is on disk (post-Init or post-Join), the daemon flips
  the flag, and the existing token-based interceptor takes over for
  every RPC.

`jacod.yaml` schema (closed set):

```yaml
data_dir: /var/lib/jaco              # raft store, snapshots, certs, wg keys
listen_addr: 0.0.0.0:7000            # cluster gRPC (TLS) for peers + remote CLI
unix_socket: /var/run/jaco/jaco.sock # local CLI control (no TLS)
wg_port: 51820                       # WireGuard UDP
acme_email: ops@example.com          # optional; empty disables ACME contact
log_level: info                      # debug | info | warn | error
ipam_pool: 10.244.0.0/16             # optional override
```

Per-subsystem `Run(ctx)` already exists for: scheduler.Scheduler,
scheduler.health.Restarter, ingress.rebuild.Reloader, ingress.challenge
HTTP handler (mounted on Caddy's :80), discovery.firewall.Reconciler.Loop,
runtime.health.Watcher (per replica). The daemon's job is `go subsys.Run
(ctx)` in the right order + WaitGroup teardown.

systemd unit (`build/jaco.service`) updates: `ExecStart=
/usr/local/bin/jacod`, no flags. The unit assumes `jacod.yaml` exists at
`/etc/jaco/jacod.yaml`; the installer (task 36) drops a template there if
none exists.

## §5 Sequence

Fresh-host cluster bring-up:

1. Operator installs the release tarball (task 36): jaco + jacod land in
   /usr/local/bin, jacod.yaml template + jaco.service install, jaco
   system user created, $DATA_DIR mode 0700.
2. `systemctl start jacod`. jacod loads jacod.yaml, finds $DATA_DIR/raft
   empty, opens the unix socket + TLS TCP listener with a self-signed
   placeholder cert (real cert exists post-Init).
3. Operator runs `sudo jaco cluster init`. CLI dials the unix socket,
   calls Cluster.Init.
4. Daemon: generates cluster id (UUID), generates Ed25519 cluster CA,
   generates node cert signed by the CA, bootstraps raft as a single-
   voter cluster, applies Command{ClusterInit} carrying cluster_id +
   CA + first operator token, writes node cert + key to disk,
   transitions to Initialized=true, swaps the TLS listener's cert.
5. RPC returns `{cluster_id, operator_token}`; CLI prints the token to
   stdout once. From here every other RPC works (token-gated for
   non-Cluster.Init/Join methods).

Adding a node:

1. Operator runs `sudo jaco node issue-join-token --identity worker-2`
   against an initialized node. Daemon raft-Applies the join token,
   returns the 32-byte hex.
2. Operator runs `sudo systemctl start jacod` on worker-2 (jacod is
   passive — listening on unix socket but nothing else yet).
3. Operator runs `sudo jaco node join --peer node-1:7000 --token <hex>`
   on worker-2. CLI dials the local unix socket, calls Cluster.Join.
4. Daemon on worker-2: dials node-1's TLS gRPC, calls Internal.NodeJoin
   with the hex token + a fresh CSR. Leader validates the join token
   (consume-once), signs the CSR, returns the cluster CA + the signed
   cert + the cluster's existing raft peer set.
5. worker-2 writes node cert + key to disk, opens its raft node and
   dials the existing peers, catches up via snapshot + log.
6. Leader raft-Applies Command{NodeJoin{hostname, address}} so the new
   node appears in state.Nodes.
7. RPC returns success; worker-2 daemon transitions to Initialized=true.

Steady-state:

8. The daemon's scheduler / restarter / rollout / runtime-health /
   ingress-rebuild / firewall-reconciler / wgmesh goroutines all run
   on every node. The scheduler and restarter self-gate on
   leader.IsLeader() so follower goroutines are harmless; runtime +
   ingress + firewall run on every node.

Shutdown:

9. `systemctl stop jacod` → SIGTERM → root context cancels → reverse-
   order teardown via WaitGroup → raft.Shutdown closes the bolt store.
   Existing containers keep running (lifecycle is observe-only on
   restart; orphan reconcile re-claims them on next boot).

## §6 Out of scope

- The five real-engine E2Es (isolation-rig, drain-node, logs-fanout,
  status-watch, ingress-acme): they're separate tasks that gain a
  running target once this slice lands.
- New gRPC services beyond Cluster.{Init, Join}; the existing surface is
  already wired.
- Hot-reload of jacod.yaml. SIGHUP could trigger a config re-read in a
  follow-up; v1 requires `systemctl restart jacod` for config changes.

> If the parent spec is ambiguous on anything this slice depends on, stop
> and update the spec. Do not invent behavior here.
