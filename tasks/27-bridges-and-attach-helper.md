Parent slice: [discovery](../slices/discovery.md), [runtime](../slices/runtime.md)
Depends on: 25, 17

# Task 27 — bridges-and-attach-helper

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Per-(deployment, network) docker bridges with labels, IPAM bound to the Subnet, teardown when the last replica leaves; `runtime_attach.BridgesForService` helper called from runtime/lifecycle.

## Tasks
- [ ] Create `internal/discovery/bridge/bridge.go` with `Ensure(ctx, d Docker, deployment, network, cidr, clusterID string) (dockerNetName string, err error)`. Docker network name: `jaco_<deployment>_<network>`. Linux bridge name (passed to docker via `com.docker.network.bridge.name`): `jaco-<dep-hash>-<net-hash>` where each hash is the first 4 hex chars of `sha1(name)` (fits 15-char kernel limit).
- [ ] Labels on the docker network: `jaco.deployment=<name>`, `jaco.network=<name>`, `jaco.cluster_id=<uuid>`, `jaco.subnet=<cidr>`.
- [ ] IPAM config: gateway = first IP of subnet (e.g. `10.42.5.1` for `10.42.5.0/24`); container IPs assigned by docker's IPAM from `.2` upward, constrained to the subnet.
- [ ] `Teardown(ctx, d Docker, deployment, network string) error`: invoked when no `ReplicaDesired` on this node references (deployment, network). Removes the docker network.
- [ ] Create `internal/discovery/runtime_attach/attach.go` with `BridgesForService(store *state.Store, deployment, service string) ([]string /*docker net names*/, error)`. Look up the service's compose `networks:` declaration via `state.Deployments`; if absent, return `["jaco_<deployment>__default"]`; otherwise return one entry per declared network.
- [ ] Modify `internal/runtime/lifecycle/lifecycle.go` (task 17): after `ContainerCreate(NetworkMode: none)`, iterate `BridgesForService` and call `NetworkConnect` for each.
- [ ] Set the container's `/etc/resolv.conf` (via Mounts or via docker engine `DNSConfig`) to `nameserver <gateway-ip>` for each attached bridge in attach order.
- [ ] Integration test (build tag `docker`): create deployment `dep1` with networks `[a, b]`; assert two bridges created with expected names + labels + subnets; delete deployment; assert teardown.

## Acceptance criteria
- [ ] `go test -tags=docker ./internal/discovery/bridge/... ./internal/discovery/runtime_attach/... -race -count=1` exits 0.
- [ ] Test asserts the bridge name length ≤ 15 chars.
- [ ] Test asserts teardown removed the docker network after delete.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
