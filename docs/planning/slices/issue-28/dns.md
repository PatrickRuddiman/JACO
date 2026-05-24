Parent spec: [Issue #28 — cross-host service discovery](https://github.com/PatrickRuddiman/jaco/issues/28) (driving spec) · repo [spec.md](../../spec.md)

# Cross-host service discovery — dns

## §1 Summary
Makes service names resolve to the right per-host container IP once the [control-plane](control-plane.md) and [datapath](datapath.md) slices have made those IPs unique and routable. Covers the DNS Manager bind-ordering race, cluster-wide answers with local-replica preference, and the `NetworkConnect` alias belt-and-suspenders. Does not allocate subnets, route packets, or set firewall rules.

## §2 Codebase reconnaissance
- DNS Manager at `internal/discovery/dns/manager.go` — already subscribes to `state.Subnets` + `state.ReplicasObserved` watches, runs one responder per `(deployment,network)` bound to the bridge gateway IP. `ensure()` (manager.go:99) calls `ListenAndServe` **once** in a goroutine; on bind failure it logs and the responder is disabled permanently — **no retry**.
- `refreshServiceMaps` (manager.go:140) builds per-scope `ServiceMap` from `ReplicasObserved` filtered to RUNNING + `last_health_at < 10s`, IP read from `obs.GetDetails()["ip"]`. Iterates **all** observed replicas (cluster-wide via raft state) — no host/locality filter. `Manager.Hostname` is available but unused in the filter.
- Responder at `internal/discovery/dns/responder.go` — `Handle`/`answerA` already shuffle A records (`rand.Shuffle`, responder.go:140). `parseInScopeName` (responder.go:175) accepts bare `<service>` and `<service>.jaco.local`; any other dotted name → forwarded upstream. Uses **`.jaco.local`**.
- **`Details["ip"]` is never populated anywhere.** `health.poll` (`internal/runtime/health/health.go:162`) calls `ContainerInspect` — whose result carries `info.NetworkSettings.Networks[<dockerNet>].IPAddress` — but writes only `exit_code` into `Details`. So `refreshServiceMaps` skips every replica (`ipStr == ""`) and the responder returns NXDOMAIN for all in-scope services today. `ReplicaObserved` carries `host` (entities.proto:133) + a `details` map.
- NetworkConnect sites pass no aliases: create-time first network at `internal/runtime/lifecycle/config.go:58` (`EndpointsConfig[net] = {}`), additional networks at `internal/runtime/lifecycle/lifecycle.go:120` (`NetworkConnect(..., nil)`).
- `compose.ContainerSpec` carries `Deployment` + `Service` (`internal/runtime/compose/spec.go:18`) — alias source. `spec.DNSServers` already set to the bridge gateway (`reconciler.go` `resolveDNSServers`).
- DNS lib is `github.com/miekg/dns`; responder is pure-Go + golden tests (`responder_test.go`, `manager_test.go`).

## §3 Decisions
1. **Bind-ordering race.** Options: backoff retry in the listener goroutine; gate on an interface AddrList readiness check. **Chosen:** backoff retry. Rationale: the issue's choice — robust to the gateway-not-ready race with no separate probe and no check-then-bind TOCTOU.
2. **Locality preference.** Options: only-local-if-any; local-first with all IPs included. **Chosen:** only-local-if-any. Rationale: eliminates the cross-host trombone whenever a healthy local replica can serve; the map is already health-filtered so local entries are live.
3. **Name scheme + alias set.** Options: adopt `.jaco.internal`; keep `.jaco.local`. **Chosen:** `.jaco.internal` + resolve `<service>.<deployment>`. Rationale: `.local` is RFC-6762-reserved for mDNS (a resolver footgun); pre-1.0 is the time to switch. Aliases become `[service, service.deployment, service.deployment.jaco.internal]`.
4. **A-record IP source.** Options: observed container IP; compute from subnet + replica index. **Chosen:** observed container IP. Rationale: the real docker-assigned IP (now globally unique post per-host-subnet fix); docker doesn't guarantee assignment order matches replica index.
5. **IP population point + multi-homed keying.** Options: per-network keys written in `health.poll`; a single `Details["ip"]`. **Chosen:** per-network keys in `health.poll`. Rationale: `poll` already inspects the container, so it has every network's IP; per-network keys give multi-homed replicas the correct IP in each scope, and the values travel in raft state so the Manager on any host sees remote replicas' IPs (a remote `ContainerInspect` isn't possible).

## §4 Contracts & shapes
**Bind retry (`internal/discovery/dns/manager.go` — `ensure`)**
- Pass the `Run` context into `ensure`. Each of the UDP and TCP listener goroutines wraps `ListenAndServe` in a loop: on a bind error, sleep with backoff 200ms→5s (capped, then steady), retry. A successful bind blocks until `Shutdown`. The loop exits when the context is cancelled or the listener is removed (`reconcileSubnets` deletes the entry + calls `Shutdown`) — distinguished from a bind error via a per-entry done signal so intentional shutdown doesn't re-bind. Log the first bind failure once per entry (existing log-once style).

**IP population (`internal/runtime/health/health.go` — `poll`)**
- After `ContainerInspect`, for each entry in `info.NetworkSettings.Networks`, write `Details["ip." + <dockerNetworkName>] = <IPAddress>` (e.g. `Details["ip.jaco_app_frontend"]`). The existing `exit_code` detail is retained. This is new behavior — nothing populated the IP before, so the responder produced no answers.

**Locality + per-network IP (`internal/discovery/dns/manager.go` — `refreshServiceMaps`)**
- For each `(scope = deployment/network)`, read the replica's IP from `Details["ip." + bridge.DockerNetworkName(deployment, network)]` — the scope's own network, so a multi-homed replica contributes the right IP per scope (not a single `Details["ip"]`).
- While iterating observed replicas, track per `(scopeKey, service)` two IP lists: `local` (where `obs.GetHost() == m.Hostname`) and `all`. After the pass, the `ServiceMap[service]` entry is `local` when non-empty, else `all`. The responder shuffles whatever it receives (unchanged).

**Responder names (`internal/discovery/dns/responder.go` — `parseInScopeName`)**
- In-scope names for a responder scoped to `(deployment, network)`: bare `<service>`; `<service>.<deployment>` (when `deployment == scope.Deployment`); `<service>.jaco.internal`; `<service>.<deployment>.jaco.internal`. All resolve to `<service>` in scope. Any other dotted name → forwarded upstream (unchanged). Replace the `.jaco.local` suffix constant with `.jaco.internal`; update `responder.go` doc comment + `responder_test.go`.

**NetworkConnect aliases (`internal/runtime/lifecycle`)**
- New helper (one func, two callsites): given a `ContainerSpec`, returns `[spec.Service, spec.Service + "." + spec.Deployment, spec.Service + "." + spec.Deployment + ".jaco.internal"]`.
- `config.go:58`: `EndpointsConfig[spec.Networks[0]] = &network.EndpointSettings{Aliases: <helper>}` instead of `{}`.
- `lifecycle.go:120`: `NetworkConnect(ctx, net, resp.ID, &network.EndpointSettings{Aliases: <helper>})` instead of `nil`.
- Effect: docker's embedded DNS (127.0.0.11) resolves same-host service names from these aliases even if the JACO responder is down — belt-and-suspenders alongside the bridge-gateway responder.

## §5 Sequence
1. datapath creates the per-host bridge (gateway `.1`) → Manager's `state.Subnets` watch fires → `reconcileSubnets` → `ensure` spawns the responder and retry-binds UDP+TCP on `gateway:53` until the gateway IP is on the bridge.
2. Reconciler starts the replica → `lifecycle` attaches each network with `Aliases=[service, service.deployment, service.deployment.jaco.internal]` → `health.poll` inspects the container, writes `Details["ip.<network>"]` for each attached network, reports RUNNING → `ReplicaObserved` written to raft.
3. Manager's `ReplicasObserved` watch fires → `refreshServiceMaps` filters RUNNING+healthy, partitions local vs cluster-wide per `(scope, service)`, applies only-local-if-any → `SetServices` on the scope's responder.
4. Container resolves `db` → resolv.conf nameserver is the bridge gateway → responder returns the local replica IP if one exists, else all cluster-wide IPs (shuffled) → cross-host resolution succeeds.
5. `db.<deployment>` / `db.<deployment>.jaco.internal` → same in-scope answer. A truly external name → forwarded upstream.
6. Replica becomes unhealthy / leaves → `ReplicasObserved` change → `refreshServiceMaps` drops it. Subnet freed → `reconcileSubnets` shuts the responder down (retry loop exits).

## §6 Out of scope
- Subnet allocation, the `host` proto/state key, pool exhaustion → **control-plane** slice.
- Bridge creation, MTU, kernel routes, WG AllowedIPs, nftables → **datapath** slice.
- A resolv.conf `search` domain for `jaco.internal` (bare `<service>` already resolves via the responder; FQDN forms resolve explicitly).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
