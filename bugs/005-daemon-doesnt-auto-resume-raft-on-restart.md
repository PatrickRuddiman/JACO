# BUG 005 — jacod doesn't auto-resume raft state on restart

## Symptom

After `systemctl restart jaco.service` on a node whose raft store
already exists on disk (`/var/lib/jaco/raft/{log.db,snapshots}`), the
new jacod process logs:

```
jacod: listening on /var/run/jaco/jaco.sock (uninitialized — run
`jaco cluster init` or `jaco node join`)
```

…and stays uninitialized. The cluster effectively halves (or worse —
if every node restarts, the cluster collapses to zero) because no
node's raft is reopened.

## Severity

**Blocking.** Production-fatal: any restart (systemd, host reboot,
even a SIGTERM-then-up cycle) kills the cluster's membership in that
node. Operator has to manually re-run cluster init or node join,
which would wipe state and re-bootstrap rather than resume.

## Root cause

Per the iter-3 of task 38 design, jacod's main() only:
  1. Loads jacod.yaml
  2. Opens the gRPC server + InitGate (initially closed)
  3. Blocks on signals

OpenRaft (which reads the existing raft store + flips InitGate) only
runs from the `Cluster.Init` and `Cluster.Join` RPC handlers. There's
no boot-time check for "raft state exists → reopen automatically."

The smoke tests in `cmd/jacod/main_test.go` and the local
single-node demo all exercise a fresh dataDir, so the gap never
surfaced.

## Fix

In `cmd/jacod/main.go::run`, after `dgrpc.New(opts)` and before
`server.Serve()`, check whether `$dataDir/raft/log.db` already exists.
If yes, resolve the hostname (same way Cluster.Init does) and call
`server.OpenRaft(hostname, opts.ClusterAddr)` so the daemon picks up
where it left off. Then immediately `server.Gate().MarkInitialized()`
so the gate is open from byte zero.

If reopening fails the daemon logs the error and continues serving
gated (operator can intervene); it does NOT delete the state on its
own.

## Status

**FIXING NOW.**
