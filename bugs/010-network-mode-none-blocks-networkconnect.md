# BUG 010 — HostConfig.NetworkMode=none blocks subsequent NetworkConnect

## Symptom

After bugs 006 + 007 fixed (image pull + bridge.Ensure), containers
get to "Created" state but never Start. Inspect shows StartedAt=zero,
ExitCode=0. Daemon log:

```
start replica hello-web-0: lifecycle.Start:
  NetworkConnect jaco_hello__default -> 240eab1302...:
  Error response from daemon: container cannot be connected to
  multiple networks with one of the networks in private (none) mode
```

## Severity

**Blocking.** Same downstream effect as previous network bugs —
containers never start.

## Root cause

`internal/runtime/lifecycle/config.go::buildConfig` sets
`hostCfg.NetworkMode = "none"`. lifecycle.Start then does
ContainerCreate → loops NetworkConnect → ContainerStart.

Docker's networking treats `NetworkMode=none` as a *terminal*
networking state — the container is meant to have no network ever.
NetworkConnect from "none" to a real network is rejected.

The correct pattern is to specify the first network at create-time
via `NetworkingConfig.EndpointsConfig`, then NetworkConnect any
additional ones at runtime.

## Fix

`buildConfig`:
- Drop `NetworkMode = "none"`.
- When `spec.Networks` is non-empty, populate
  `netCfg.EndpointsConfig[spec.Networks[0]] = &network.EndpointSettings{}`.
- `HostConfig.NetworkMode` defaults to "default" (bridge) when no
  user-supplied networks; lifecycle.Start NetworkConnects the rest.

`lifecycle.Start`:
- The NetworkConnect loop now skips `spec.Networks[0]` because it was
  attached at create-time.

## Status

**FIXING NOW.**
