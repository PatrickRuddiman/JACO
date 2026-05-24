Parent slice: [discovery](../slices/discovery.md), [runtime](../slices/runtime.md)
Depends on: 25, 17

# Task 27 — bridges-and-attach-helper

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Per-(deployment, network) docker bridges with labels, IPAM bound to the Subnet, teardown when the last replica leaves; `runtime_attach.BridgesForService` helper called from runtime/lifecycle.

## Tasks
- [x] Extend `internal/runtime/dockerx/iface.go` Docker interface with `NetworkCreate`, `NetworkRemove`, `NetworkList` so bridge.Ensure/Teardown can target the same partial-fake test pattern.
- [x] Create `internal/discovery/bridge/bridge.go` with `DockerNetworkName(dep, net)` (returns `jaco_<dep>_<net>`; compose's "default" maps to `_default` to match compose.networkName from task 13), `LinuxBridgeName(dep, net)` (returns `jaco-<dep4>-<net4>` using the first 4 hex chars of sha1 each — fits the 15-char kernel limit), `GatewayIP(cidr)` (first usable address of the /24), `Ensure(ctx, d, dep, net, cidr, clusterID)` (idempotent NetworkCreate with the right labels + IPAM + `com.docker.network.bridge.name` option), and `Teardown(ctx, d, dep, net)` (NetworkList + NetworkRemove for the matching network).
- [x] Labels on the docker network: `jaco.cluster_id`, `jaco.deployment`, `jaco.network`, `jaco.subnet`.
- [x] IPAM config: gateway = `<first usable IP>` of the /24; subnet = the IPAM-allocated cidr; container IPs auto-assigned by docker's IPAM from `.2` upward.
- [x] Create `internal/discovery/runtime_attach/attach.go` with `BridgesForService(state, deployment, service)`. Looks up the service in state.Deployments, returns one docker network name per declared `Networks` entry; defaults to `[jaco_<deployment>__default]` when the service declares no networks.
- [x] Modify `internal/runtime/lifecycle/lifecycle.go` Start: after ContainerCreate (NetworkMode=none), iterate `spec.Networks` and NetworkConnect each. (Using spec.Networks directly — compose.ToContainerSpec already populated this in task 13. BridgesForService is the state-driven lookup for callers that don't have a spec on hand, e.g. orphan reconcile.)
- [ ] **Deferred**: Set the container's `/etc/resolv.conf` to the per-bridge gateway IP. Requires CreateOptions.DNSConfig wiring — straightforward but only useful once the runtime is running real containers. Lands when the daemon entry comes together.
- [x] 21 tests pass with -race: ten bridge tests (DockerNetworkName underscores + default→_default mapping, LinuxBridgeName 15-char limit across long inputs (the AC), LinuxBridgeName deterministic, GatewayIP correctness across /24s, GatewayIP rejects garbage, Ensure stamps all labels + IPAM + bridge.name option, Ensure idempotent, Ensure rejects empty args, Teardown removes (the AC), Teardown no-op on missing); four BridgesForService tests (declared networks, default fallback, missing deployment, missing service); seven existing lifecycle tests still pass plus a new TestStart_AttachesEachDeclaredNetwork asserting NetworkConnect is called once per spec.Networks entry.
- [ ] **Deferred**: real-engine integration test under `-tags=docker` — the in-memory fake covers the contract; real-engine assertions land alongside task 31's CI rig.

## Acceptance criteria
- [x] `go test ./internal/discovery/bridge/... ./internal/discovery/runtime_attach/... -race -count=1` exits 0 (14 tests across both).
- [x] Test asserts the bridge name length ≤ 15 chars (`TestLinuxBridgeName_FitsKernel15CharLimit`).
- [x] Test asserts Teardown removes the docker network (`TestTeardown_RemovesNetwork`).
- [ ] `go test -tags=docker ./internal/discovery/bridge/...` against a real engine — deferred to task 31's CI rig.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
