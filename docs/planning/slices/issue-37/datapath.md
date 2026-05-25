Parent spec: [Issue #37 — ingress: add TCP port-forwarding and reject compose-level 80/443 binds](https://github.com/PatrickRuddiman/jaco/issues/37) (driving spec) · repo [spec.md](../../spec.md) · sibling [control-plane](control-plane.md)

# TCP ingress — datapath

## §1 Summary

The per-node north-south TCP forwarder. Every node listens on every declared published port and proxies each connection to an eligible replica's overlay IP wherever it runs in the cluster, reusing the embedded Caddy reload loop and the overlay-IP upstream model the HTTP path already proves. Consumes the `TCPRoute` entity the [control-plane](control-plane.md) slice defines; owns no state of its own.

## §2 Codebase reconnaissance

- HTTP forwarder is embedded Caddy: `config.BuildCaddyConfig` (`internal/ingress/config/config.go:85`) is a pure state→Caddy-JSON function; the root `apps` map sets `apps.http.servers.jaco.listen = [":80", ":443"]` (`config.go:179-197`) and omits `apps.tls` when there are no TLS policies (`config.go:191`).
- Upstreams are **overlay IPs anywhere in the cluster**: `ingressBuilder` (`internal/daemon/grpc/ingress.go:33`) builds `ServiceMeta.ReplicaIPs` (replica id → per-network overlay IP) from `obs.GetDetails()["ip."+bridge.DockerNetworkName(...)]` (`ingress.go:66-89`), and `buildUpstreams` dials `<ip>:<port>` (`config.go:203-218`).
- Eligibility rule lives in `config.isEligible` (`config.go:362`): `state == "running" && now-last_health_at < HealthFreshness (10s)` (`config.go:24`).
- Cross-host datapath is already built (issue #28): daemon-originated north-south traffic is sourced from the node's WG mesh IP and admitted `mesh→pool` on the destination host — `firewall.EnsureOverlayExempt` + the `meshSubnet` const (`internal/discovery/firewall/overlay.go:11-54`). So a node with no local replica can proxy to a remote one today.
- nftables `input` chain is **policy-accept with no rules** (`internal/discovery/firewall/render.go:124-126`) — a listener on any port needs no firewall hole.
- Reload loop: `rebuild.Reloader` (`internal/ingress/rebuild/rebuild.go`) subscribes to Routes/ReplicasObserved/Certs/ChallengeTokens (`rebuild.go:78-85`), debounces 200ms, rebuilds, and only calls the loader when bytes changed (keeps `lastCfg`, `rebuild.go:59-72`).
- Loader: `ingressLoaderEmbedded` (`internal/daemon/grpc/ingress.go:117`) calls `caddy.Load` but **skips when the config lacks `"reverse_proxy"`** (`ingress.go:120`, a bug-009 once-per-second restart guard). Caddy standard modules are registered via `_ "github.com/caddyserver/caddy/v2/modules/standard"` (`ingress.go:18`).
- `go.mod`: `github.com/caddyserver/caddy/v2 v2.11.3` (`go.mod:6`). `github.com/mholt/caddy-l4` is **not** a dependency yet. The repo already uses `net.Listen("tcp", ...)` for its gRPC listeners (`internal/daemon/grpc/server.go:258`).
- Reloader wiring: constructed + run at `internal/daemon/grpc/server.go:599-606`, gated on `caddyAvailable()`.

## §3 Decisions

1. **Forwarder implementation.** Options: caddy-l4 (extend embedded Caddy); standalone `net.Listener`+`io.Copy`; nftables DNAT. **Chosen:** caddy-l4. Rationale: reuses the proven `caddy.Load` reload loop and the overlay-IP upstream/eligibility model already in `BuildCaddyConfig`; the L4 block is emitted from the same pure function and loaded in the same atomic swap — near-zero new selection/LB code. One new (same-author, idiomatic) dependency.
2. **Cluster-wide listeners.** Options: listen only on nodes hosting a local replica; listen on every node for every route. **Chosen:** every node emits a layer4 listener for **every** `TCPRoute` in cluster state, with upstreams = eligible overlay IPs anywhere. Rationale: the issue requires "reachable on every node regardless of where scheduled"; a node with no local replica proxies over the WG mesh (sourced from its mesh IP, admitted `mesh→pool` by `firewall/overlay.go`) — identical to how Caddy serves HTTP on every node.
3. **Load balancing.** Options: round-robin; random (HTTP parity); source-IP hash. **Chosen:** round-robin per new connection (each connection then pins to its chosen replica for its lifetime). Rationale: TCP services carry a small number of long-lived connections where random clumps; round-robin spreads them evenly. caddy-l4 supports it directly.
4. **Rolling-update drain.** Options: graceful (exclude from new, let open finish); hard cut on reload. **Chosen:** graceful. Rationale: a retiring-but-healthy replica is dropped from new connections on rebuild; established connections run until the peer closes or the scheduler tears the container down after its drain window (the effective grace). Mirrors the HTTP "in-flight allowed to complete" promise and relies on caddy-l4's graceful config swap — no connection bookkeeping. A crashed replica needs no handling: its sockets are already severed and reconnects land on survivors.
5. **Port-bind failure handling.** Options: probe-and-skip per port; atomic + keep last-good. **Chosen:** probe-and-skip in the daemon builder. Rationale: `caddy.Load` is atomic, so one un-bindable port would otherwise stall all ingress (incl. HTTP) on that node; the builder test-binds each port and omits + logs the un-bindable ones so the rest still load. A rare probe→load TOCTOU race fails the load atomically, the Reloader keeps last-good, and the next rebuild re-probes and converges.
6. **Upstream health.** Options: reuse `running + fresh`; add a caddy-l4 active TCP-connect probe. **Chosen:** reuse `running + fresh` only (`config.isEligible`), no active health. Rationale: the issue mandates the same eligibility rule the HTTP path uses; keeps L4 and L7 behavior identical.

## §4 Contracts & shapes

**Dependency**
- Add `github.com/mholt/caddy-l4 v0.1.1`. Its `go.mod` requires exactly `caddyserver/caddy/v2 v2.11.3` (JACO's current pin — no MVS upgrade) and `go 1.25.0` (JACO is on `go 1.25.10`), so module resolution is clean. Register its modules at the daemon edge alongside the existing standard-modules import (`internal/daemon/grpc/ingress.go:18`), so `caddy.Load` resolves the `layer4` app + `proxy` handler.

**`config.BuildCaddyConfig` (`internal/ingress/config/config.go`) — stays pure**
- New input parameter: `tcpRoutes []TCPRoute`, where `TCPRoute{PublishedPort int, Deployment, Service string, ContainerPort int}` (the daemon projects these from `state.TCPRoutes`).
- When `len(tcpRoutes) == 0`, the `apps.layer4` key is omitted entirely — existing golden files (`internal/ingress/config/testdata/*.json`) stay byte-identical (parallel to the `apps.tls` omission at `config.go:191`).
- Otherwise emit `apps.layer4.servers` as a map, one server per published port keyed `tcp_<port>`: `listen = [":<published_port>"]`; one route whose `proxy` handler carries `upstreams` = `{dial: ["<overlayIP>:<containerPort>"]}` for each **eligible** replica of `(deployment, service)` (same `healthyByService` + `services` maps the HTTP path uses) and a round-robin load-balancing selection policy. Upstream set empty ⇒ the listener still exists (refuses/holds per caddy-l4 default) but has no backends — same shape as an HTTP route with zero healthy upstreams.
- Deterministic ordering: servers emitted in ascending published-port order; upstreams sorted by replica id (matches `config.go:103-106`).

**Daemon builder (`internal/daemon/grpc/ingress.go` — `ingressBuilder`)**
- Gather `[]config.TCPRoute` from `state.TCPRoutes.List()`, joining each `TCPRoute` to the same `ServiceMeta`/eligible-replica data already computed for HTTP (reuse the `services` map at `ingress.go:66-89`; upstream IP = the per-network overlay IP, dial port = `TCPRoute.container_port`).
- **Bind probe:** for each candidate published port, `net.Listen("tcp", ":<port>")` then immediately close; on error, drop that `TCPRoute` from the set passed to `BuildCaddyConfig` and log `ingress: tcp port <port> unbindable on this node, skipping (degraded): <err>`.
- Pass the surviving `tcpRoutes` into `BuildCaddyConfig`.

**Loader gate (`internal/daemon/grpc/ingress.go` — `ingressLoaderEmbedded`)**
- The "has a real route" guard (`ingress.go:120`) must also fire for layer4 configs: load when the config contains `"reverse_proxy"` **or** a layer4 `"proxy"` handler, else a TCP-only deployment (ports, no HTTP routes) would never load.

**Reload loop (`internal/ingress/rebuild/rebuild.go` — `Reloader.Run`)**
- Add a `TCPRoutes` subscription (`r.brokers.TCPRoutes.Subscribe()`) to the select loop (`rebuild.go:78-115`), feeding the same debounce/rebuild path as Routes. No other change — the existing byte-equality short-circuit (`rebuild.go:59-66`) already suppresses no-op reloads.

**Reachability contract (unchanged infra)**
- Listener bind needs no firewall change (`input` chain policy-accept, `render.go:124`).
- Cross-host proxy needs no firewall change: host-originated mesh traffic is already admitted `mesh→pool` (`firewall/overlay.go:46`).

## §5 Sequence

Steady-state forward (replica on another node):
1. Client connects to node B on `5432`; node B has no local replica.
2. caddy-l4 layer4 server `tcp_5432` accepts; `proxy` round-robins to an eligible upstream `<overlayIP>:<containerPort>` on node A.
3. Node B dials the overlay IP; the packet egresses `jaco0` sourced from B's WG mesh IP; node A admits it `mesh→pool` and delivers to the replica's bridge.
4. Sockets are spliced both ways for the connection's lifetime (pinned to that replica).

Replica dies (3→2 replicas):
1. Health watcher marks the dead replica not-`running` (or its heartbeat goes stale >10s); broker fires.
2. Reloader debounces 200ms, rebuild drops it from `healthyByService`, `BuildCaddyConfig` emits the listener with the 2 survivors, `caddy.Load` swaps.
3. New connections round-robin across the 2 survivors; the dead replica's connections were already severed by its container exit.

Rolling update (retire a healthy replica):
1. Scheduler removes the retiring replica's eligibility; broker fires; rebuild excludes it from new connections.
2. Its established connections continue until the peer closes or the scheduler tears the container down after the drain window.

Route delete (deployment delete):
1. Control-plane cascade removes the deployment's `TCPRoute`s; `TCPRoutes` broker emits Removed.
2. Rebuild emits a config without those layer4 servers; `caddy.Load` closes the listeners cluster-wide.

Port-bind conflict on one node:
1. Builder's probe finds `:5432` already held by an operator service on node C → drops it, logs degraded; node C loads the rest.
2. Nodes A/B still serve `5432`; the deployment is reachable via them. (A probe→load race that slips through fails the atomic load; Reloader keeps last-good; next rebuild re-probes and converges.)

## §6 Out of scope

- The `TCPRoute` proto/state/derivation, collision policy, and `reserved_port` validation → **control-plane** slice.
- UDP forwarding (issue out-of-scope; caddy-l4 supports UDP but no caller exists).
- PROXY-protocol injection, mTLS/TLS termination at L4, per-port ACL/auth (issue out-of-scope).
- HTTP/S behavior — `apps.http`/`apps.tls` emission is unchanged; this slice only adds the `apps.layer4` block.
- Active TCP-connect health probing (decision 6: eligibility is `running + fresh` only).
- The scheduler's rolling-update drain timer itself (owned by the scheduler slice); this slice only stops sending new connections to a retiring replica.

## §7 Open questions

- **caddy-l4 version pin — RESOLVED.** `github.com/mholt/caddy-l4 v0.1.1` requires exactly `caddy/v2 v2.11.3` and `go 1.25.0`; both match JACO. Recorded in §4.
- **Active-health gap (accepted).** With `running + fresh` only, a replica the watcher still reports healthy but that is briefly TCP-refusing keeps receiving new connections until the next 10s health window — identical to the HTTP path's exposure, accepted for parity. Flagged only so it isn't mistaken for a regression.

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
