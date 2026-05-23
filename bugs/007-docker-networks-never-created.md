# BUG 007 — docker networks never created before NetworkConnect

## Symptom

After bug 006 fix landed (image pulls now work), the next reconcile
attempt fails on NetworkConnect:

```
start replica hello-web-0: lifecycle.Start: ContainerStart eee3e7b1...:
  Error response from daemon: network jaco_hello__default not found
```

Container is created (good) but can't attach to `jaco_hello__default`
because that network doesn't exist on the docker engine.

## Severity

**Blocking.** Same effect as bug 006: containers never start.

## Root cause

`ipam.EnsureSubnets` (wired into Deploy.Apply by task 25) allocates a
`Subnet{deployment, network, cidr}` entity in raft so every node sees
the allocation. But nothing on each node actually calls
`bridge.Ensure(...)` to create the matching docker network on the
local engine. The bridge package has the helper; it's just never
invoked from the runtime path.

The DNS Manager from iter 31 subscribes to state.Subnets but only
tries to bind to the gateway IP — assuming the bridge already exists.
Same gap on its side.

## Fix

`internal/runtime/reconciler/reconciler.go::startReplica`: before
`lifecycle.Start`, iterate spec.Networks, decode each docker network
name back to its (deployment, network) pair via
`bridge.NetworkNameFromDockerName`, look up state.Subnets, and call
`bridge.Ensure(...)` to create the docker network on the local engine
(idempotent — returns the existing network id when the network is
already there).

`bridge.Ensure` already takes a dockerx.Docker, so the reconciler can
pass its existing `r.docker` handle.

## Status

**FIXING NOW.**
