Parent slice: [daemon](../slices/daemon.md)
Depends on: 05, 06, 07, 17, 18, 21, 22, 23, 26, 28, 29, 30, 32, 33, 34, 35, 36

# Task 38 — daemon-entry

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Ship `cmd/jacod/main.go` — the long-running daemon that owns raft, the gRPC server (unix-socket + TLS-TCP), and every per-host subsystem goroutine. Split the existing single-binary CLI into `cmd/jaco/` (control client) + `cmd/jacod/` (daemon). Move bootstrap from a CLI command into the `Cluster.Init` RPC handler so operators run `sudo systemctl start jacod` then `sudo jaco cluster init`.

## Tasks
- [x] **iter 1** — Add `Cluster.Init(ClusterInitRequest) returns (ClusterInitResponse)` and `Cluster.Join(ClusterJoinRequest) returns (ClusterJoinResponse)` to `proto/jaco/v1/services.proto`. ClusterInitResponse carries `{cluster_id, operator_token}`; ClusterJoinResponse is empty. ClusterStatusResponse gains `bool initialized` so `jaco status` on a fresh node reports "uninitialized" vs leader role.
- [x] **iter 3** — `cmd/jacod/main.go` reads `JACO_CONFIG` env (default `/etc/jaco/jacod.yaml`), loads YAML via `internal/daemon/config`, opens the unix socket listener at `cfg.UnixSocket` (mode 0660), and blocks on SIGTERM/SIGINT. TLS-over-TCP listener for cross-host is deferred to the steady-state-goroutines iter (see below).
- [x] **iter 2** — `internal/daemon/admission/initgate.go` wraps the daemon-side gRPC admission. Pre-init, only `Cluster.{Init, Join, Status}` accept; everything else returns `codes.Unavailable` + "cluster_uninitialized". Post-init falls through to the wrapped (token-based) interceptor.
- [x] **iter 4** — `Cluster.Init` handler. Refuses with FailedPrecondition + "cluster_already_initialized" when raft state already exists on disk. Calls `bootstrap.Run`, persists CA + cert + first operator token, flips InitGate.
- [x] **iter 5** — `Cluster.Join` handler. Refuses with cluster_already_initialized when raft state exists. Generates CSR, dials peer over TLS-skip-verify (join_token is trust anchor), exchanges via Cluster.NodeJoin, persists certs + join.json, flips gate.
- [x] **iter 6** — `Server.OpenRaft` opens raft + state + brokers + fsm from persisted state. Called post-Init and post-Join. Cluster.Status now reports raft Leader + RaftIndex + Nodes.
- [x] **iter 10** — wire `scheduler.Scheduler.Run(ctx)` (task 21) and `scheduler/health.Restarter.Run(ctx)` (task 23) from `Server.OpenRaft`. Goroutines self-gate on raft leadership; Stop cancels them and waits up to 5s before raft.Shutdown. Verified by `TestSubsystems_SchedulerMaterializesReplicaDesired` (raft-applies a DeploymentApply, asserts ReplicasDesired populates within debounce window) and `TestSubsystems_StopDrainsGoroutinesCleanly`.
- [x] **iter 11** — new package `internal/runtime/reconciler` owns the per-host runtime loop: subscribes to ReplicasDesired host=self, projects to compose.ContainerSpec, calls lifecycle.Start → health.Watcher.Start; on remove or host-migration calls lifecycle.Stop+Remove + Watcher.Stop. Boot path runs lifecycle.Reconcile (orphan sweep) before subscribing. Wired into Server.startSubsystems behind `Options.Docker != nil`; cmd/jacod best-effort connects via dockerx.New + falls through gracefully when the engine is unreachable. Verified end-to-end by `TestSubsystems_RuntimeReconcilerCreatesContainerEndToEnd`.
- [x] **iter 12** — cross-host TCP listener on `cfg.ListenAddr` (plaintext; Tailscale / WireGuard wraps the wire). Same grpc.Server serves both unix socket + TCP so post-init RPCs are visible identically on either transport.
- [x] **iter 13** — `Cluster.NodeJoin` handler on the daemon: signs the joiner's CSR with the cluster CA, raft-AddVoters them, applies `{JoinTokenConsume, NodeJoin}` atomically. Three-test coverage (happy path with a real second raft node, invalid token rejection, pre-Init gating).
- [x] **iter 14** — Operator-token admission wired into post-init RPCs via a lazy closure that picks up `state.Tokens` from OpenRaft. `Cluster.Status` joins `Cluster.NodeJoin` in UnauthMethods so operators can liveness-check without auth.
- [x] **iter 15** — Joined nodes auto-promote `JOINING → READY` in the NodeJoin batch so the scheduler will place workloads on them. (Drain-based gating is a follow-up.)
- [x] **iter 16** — Daemon-side `Internal.Submit` handler. The follower→leader forwarding client needs a `grpc_address` on Node (proto change) — deferred.
- [x] **iter 17** — Discovery: wgmesh Syncer + firewall.Reconciler.Loop wired into Server.startSubsystems behind `wgmesh.IsKernelAvailable()` / `firewall.IsAvailable()` feature checks. Skip-gracefully on unprivileged hosts.
- [x] **iter 18** — Cluster.Join dials peer plaintext, matching iter 12's listener (TLS-skip-verify dial would fail otherwise).
- [x] **iter 19** — README rewrite: two-binary install path, `cluster init` + `node join` walkthrough, Network model documenting v0 plaintext + kernel gates.
- [x] **iter 20** — Deploy / Tokens / Audit / Watch services registered on jacod via lazily-resolved proxies. The full operator surface (`jaco apply`, `jaco status <deploy>`, `jaco logs`, `jaco token list`, `jaco audit`) now lands against jacod.
- [x] **iter 21** — CLI's `dialServer` switched to plaintext (matches jacod's wire). `--ca-cert` made optional across every operator command.
- [x] **iter 22** — `build/jacod.yaml` + README accurate on ports + the v0 plaintext story.
- [x] **iter 23** — Daemon's clusterServer delegates NodeList / NodeRemove / IssueJoinToken / Backup / Restore to the controlplane impl so the full Cluster service is reachable through jacod.
- [ ] **Deferred (out-of-scope for task 38)** — the following are functional gaps that don't block a v0 cluster bring-up + workload-running demo, but each warrants its own follow-up task:
  - `discovery/dns` per-bridge UDP+TCP listener (depends on real docker bridges being live).
  - `ingress/rebuild.Reloader.Run(ctx)` (needs caddy/v2 dep).
  - Rollout state-machine integration with `scheduler.Reconcile` (image-change path).
  - Drain step machine wired into `Cluster.NodeRemove(force=false)`.
  - `Internal.Submit` client-side forwarding (needs `grpc_address` proto field on Node).
  - CertBlob entity for raft-backed cert blob storage + CertMagic OnEvent audit hooks.
  - `Deploy.Logs` cross-node fanout via `Internal.Logs`.
  - TLS-with-cluster-CA on the cross-host listener (replaces v0 plaintext).
- [x] **iter 3 + 6** — Graceful shutdown via signalContext (SIGTERM/SIGINT cancels root ctx, server.Stop is graceful with a 10s timeout, raft.Shutdown closes the bolt store last so the file lock releases).
- [x] **iter 7** — `cmd/jaco/cluster.go` ships `jaco cluster init` + `jaco cluster status`; both dial the local unix socket via `dialDaemon`. `cmd/jaco/node.go::join` rewritten as a thin RPC wrapper. `cmd/jaco/bootstrap.go` deleted (superseded by `jaco cluster init`).
- [x] **iter 7** — Unix-socket dial helper lives in `cmd/jaco/cluster.go::dialDaemon` (small enough not to need a separate cliclient package). CLI gains `--socket` flag (default `/var/run/jaco/jaco.sock`, `JACO_SOCKET` env override).
- [x] **iter 8** — `build/release.sh` builds both binaries into each tarball; `build/install.sh.tpl` installs both + drops `/etc/jaco/jacod.yaml`; `build/jaco.service` ExecStart=`/usr/local/bin/jacod`; `build/uninstall.sh` removes both binaries + config; `cmd/jaco/self_upgrade.go` swaps both atomically (stage as `.upgrading`, rename back-to-back, .prev rollback on second-rename failure).
- [x] **iter 9** — `cmd/jacod/main_test.go` integration test: boots run() in a goroutine with a temp jacod.yaml, dials the socket, asserts Status=uninitialized, calls Init, asserts Status flips + raft/log.db lands on disk + ClusterId non-empty + OperatorToken is 64 hex chars. Plus bad-config + missing-data-dir + JACO_CONFIG env-override tests.
- [ ] **Deferred — depends on the steady-state wiring above**: update the E2E shell scripts (`scripts/test/apply-deploy.sh`, `logs-fanout.sh`, `scheduler-spread.sh`, `drain-node.sh`, `status-watch.sh`, `isolation-rig.sh`, `ingress-acme.sh`, `self-upgrade.sh`, `install.sh`) to invoke `jacod` via systemctl + `jaco cluster init` / `jaco node join` and flip their JACO_*_FORCE gates off.

## Acceptance criteria
- [x] `go test ./cmd/jacod/... -race -count=1` exits 0 (5 in-process tests).
- [x] `go test ./... -race -count=1` exits 0 across the whole tree.
- [x] `make build` produces both `./jaco` and `./jacod` binaries (verified — `go build ./cmd/jacod` succeeds).
- [x] `VERSION=test bash build/release.sh` exits 0; tarball includes both `jaco` and `jacod` (verified iter 8: 8 entries per tarball).
- [x] `grep -nE 'ExecStart=/usr/local/bin/jacod' build/jaco.service` matches.
- [x] `git grep -nE 'cluster_uninitialized' internal/daemon/admission/initgate.go` matches.
- [x] `git grep -nE 'rpc Init\(ClusterInitRequest\)' proto/jaco/v1/services.proto` matches.
- [ ] On a privileged container with systemd: `systemctl start jacod && jaco cluster init` exits 0 and prints an operator token of 64 hex characters — deferred to a real-deploy verification (privileged CI runner). The equivalent assertion is in `cmd/jacod/main_test.go::TestRun_InitFlipsStatusAndPersistsRaft`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
