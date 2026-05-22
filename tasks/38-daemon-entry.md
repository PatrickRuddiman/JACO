Parent slice: [daemon](../slices/daemon.md)
Depends on: 05, 06, 07, 17, 18, 21, 22, 23, 26, 28, 29, 30, 32, 33, 34, 35, 36

# Task 38 — daemon-entry

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Ship `cmd/jacod/main.go` — the long-running daemon that owns raft, the gRPC server (unix-socket + TLS-TCP), and every per-host subsystem goroutine. Split the existing single-binary CLI into `cmd/jaco/` (control client) + `cmd/jacod/` (daemon). Move bootstrap from a CLI command into the `Cluster.Init` RPC handler so operators run `sudo systemctl start jacod` then `sudo jaco cluster init`.

## Tasks
- [ ] Add `Cluster.Init(InitRequest) returns (InitResponse)` and `Cluster.Join(JoinRequest) returns (JoinResponse)` to `proto/jaco/v1/services.proto`. Regenerate via `make proto`. InitResponse carries `{cluster_id, operator_token}`; JoinResponse is empty.
- [ ] Create `cmd/jacod/main.go`. Reads `JACO_CONFIG` env (default `/etc/jaco/jacod.yaml`). Loads the YAML into a typed config struct (`internal/daemon/config/config.go`). Opens the unix socket listener at `config.UnixSocket` (default `/var/run/jaco/jaco.sock`, mode 0660, owner root, group jaco). Opens the TLS gRPC listener at `config.ListenAddr` using a self-signed bootstrap cert when no `$config.DataDir/node/<hostname>.crt` exists yet; swaps to the cluster-CA-signed cert after Init/Join.
- [ ] Create `internal/daemon/admission/initgate.go` — a gRPC interceptor wrapping the existing `internal/controlplane/admission` interceptor. When the daemon's `Initialized` atomic flag is false, only `/jaco.v1.Cluster/Init`, `/jaco.v1.Cluster/Join`, and `/jaco.v1.Cluster/Status` accept (no bearer required on the unix socket); everything else returns `codes.Unavailable` + typed `pb.Error{code:"cluster_uninitialized"}`. When the flag is true, fall through to the existing token-based interceptor unchanged.
- [ ] Implement `Cluster.Init` handler. Refuse with `codes.FailedPrecondition` + `cluster_already_initialized` when raft state already exists on disk. Otherwise call `internal/controlplane/bootstrap.Run` (existing library — task 05) which generates cluster id, CA, node cert, raft-bootstraps as single voter, raft-Applies `Command{ClusterInit}` carrying the CA + first operator token. Flip the daemon's `Initialized` flag to true. Swap the TLS listener's cert to the new cluster-CA-signed cert. Return `{cluster_id, operator_token}`.
- [ ] Implement `Cluster.Join` handler. Refuse with `cluster_already_initialized` when raft state already exists. Otherwise: dial `peer_addr` over TLS (skip-verify for the bootstrap TLS handshake — the join token is the trust anchor); call the existing `Cluster.NodeJoin` RPC on the peer (task 07) passing the join_token + a freshly-generated CSR; receive back the cluster CA + signed node cert + raft peer set; write node.{key,crt} to disk; open the raft node with the received peer set as the existing voter list; flip `Initialized` to true; swap TLS cert.
- [ ] Wire the steady-state goroutines once `Initialized=true` flips:
  - `scheduler.Scheduler.Run(ctx)` (task 21)
  - `scheduler/health.Restarter.Run(ctx)` (task 23)
  - `discovery/firewall.Reconciler.Loop(ctx)` (task 30) — only if `firewall.IsAvailable()` returns nil; otherwise log + skip (degraded-mode operation).
  - `discovery/wgmesh.Sync` (task 26) — only if kernel WG is present; otherwise log + skip.
  - `ingress/rebuild.Reloader.Run(ctx)` (task 34) — wires `config.BuildCaddyConfig` (task 32) as the Builder and `caddy.Load` as the Loader once the caddy v2 dep lands.
  - `runtime/lifecycle.Reconcile` orphan sweep on boot (task 17).
  - `runtime/health.Watcher` per-replica goroutines (task 18) — spawned by a watch loop on `state.ReplicasDesired` filtered to `host==self`.
  - `discovery/dns` per-bridge UDP+TCP listener (task 29).
- [ ] Implement graceful shutdown: SIGTERM / SIGINT cancels the root context; a `sync.WaitGroup` joins every subsystem in reverse start order; the raft node's `Shutdown()` closes the bolt store last so the file lock is released.
- [ ] Move `cmd/jaco/bootstrap.go` to `cmd/jaco/cluster.go` and rewrite as a thin gRPC client: `jaco cluster init [--cluster-name <n>]` dials the local unix socket, calls `Cluster.Init`, prints the operator token to stdout. Add `jaco cluster status` printing initialized / role from `Cluster.Status`.
- [ ] Rewrite `cmd/jaco/node.go::join` to RPC `Cluster.Join` against the local unix socket; remove the direct raft-dial code from the CLI side.
- [ ] Extend `internal/cliclient` (task 11) with a unix-socket dial helper that uses no TLS and no bearer (the unix socket is the trust boundary). CLI commands gain a `--socket <path>` flag (default `/var/run/jaco/jaco.sock`); when `--server` is unset, fall through to the socket.
- [ ] Update `build/release.sh` (task 35) to build both binaries into each tarball: `go build -o $stage/jaco ./cmd/jaco` and `go build -o $stage/jacod ./cmd/jacod`.
- [ ] Update `build/install.sh.tpl` (task 36) to install both binaries to `$JACO_PREFIX/bin/`, drop a `jacod.yaml` template at `/etc/jaco/jacod.yaml` mode 0644 if none exists, and adjust the "already installed" / upgrade paths to compare `jacod --version`.
- [ ] Update `build/jaco.service` (task 36) — `ExecStart=/usr/local/bin/jacod`; drop the `Environment=JACO_DATA_DIR=...` line in favor of the config file (the daemon reads `JACO_CONFIG` for the path).
- [ ] Update `internal/packaging/verify.go` + `cmd/jaco/self_upgrade.go` (task 37) so the swap step renames both `jaco` and `jacod` atomically (stage both as `.upgrading`, rename both back-to-back).
- [ ] Update `build/uninstall.sh` to remove `jacod` alongside `jaco`.
- [ ] Add a `cmd/jacod/main_test.go` integration test exercising the full flow against an in-process daemon: start jacod with an empty data dir; verify `Cluster.Status` returns `INITIALIZED=false` over the unix socket; call `Cluster.Init`; verify the operator token is returned + Status flips to true; raft state lands on disk. Run with `go test -race -count=1`.
- [ ] Update the deferred E2E shell scripts (`scripts/test/apply-deploy.sh`, `logs-fanout.sh`, `scheduler-spread.sh`, `drain-node.sh`, `status-watch.sh`, `isolation-rig.sh`, `ingress-acme.sh`, `self-upgrade.sh`, `install.sh`) to invoke `jacod` via systemctl + use `jaco cluster init` / `jaco node join` for bring-up. Flip their `JACO_*_FORCE=1` gates off.

## Acceptance criteria
- [ ] `go test ./cmd/jacod/... -race -count=1` exits 0; the in-process daemon test asserts Cluster.Init flips state from uninitialized to initialized and persists raft state.
- [ ] `go test ./... -race -count=1` exits 0 across the whole tree (regression).
- [ ] `make build` produces both `./jaco` and `./jacod` binaries.
- [ ] `VERSION=test bash build/release.sh` exits 0; `tar -tzf dist/jaco-test-linux-amd64.tar.gz` includes both `jaco-test-linux-amd64/jaco` and `jaco-test-linux-amd64/jacod`.
- [ ] `grep -nE 'ExecStart=/usr/local/bin/jacod' build/jaco.service` matches.
- [ ] `git grep -nE 'cluster_uninitialized' internal/daemon/admission/initgate.go` matches.
- [ ] `git grep -nE 'rpc Init\(InitRequest\)' proto/jaco/v1/services.proto` matches.
- [ ] On a privileged container with systemd: `systemctl start jacod && jaco cluster init` exits 0 and prints an operator token of 64 hex characters.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
