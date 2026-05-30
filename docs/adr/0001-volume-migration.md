# ADR 0001: Volume migration for stateful services

- **Status:** proposed
- **Date:** 2026-05-30
- **Issue:** #91

---


A **leader-driven stop-ship-start** primitive that moves a named volume's bytes across the existing wireguard mesh on the same control flow that today does a rollout move. No new transport, no new storage abstraction in v1 — the goal is to delete the host-pin workaround in the bench manifests, not to ship a CSI layer.

## Model

### Volume identity

A volume is identified across moves by the triple

```
(deployment_id, service_name, volume_name)
```

where `volume_name` is the compose-level name from the service's `volumes:` block (the left side of `pgdata:/var/lib/postgresql/data`). On disk it materializes today as a local docker volume; under this design it materializes as a managed directory the daemon owns:

```
/var/lib/jaco/volumes/<deployment_id>/<service>/<volume_name>/
```

bind-mounted into the container instead of using `docker volume create`. Owning the path on the host is what makes ship-from-source and ship-to-dest possible — the daemon needs a file-tree to snapshot.

A migration step on the cluster state machine is keyed by that triple plus the source/dest node ids and a monotonic `move_id` so retries are idempotent.

### Move lifecycle

Five phases on the cluster state machine; each is a separate raft entry so a leader change in the middle of a move resumes cleanly:

```
PLAN  → state has (move_id, deployment, service, volume, src, dst); replica still running on src
QUIESCE → src daemon stops the replica (graceful, configured StopSignal/StopTimeout — those are already honored after PR #125)
SHIP  → src daemon streams the volume directory to dst over the wg mesh; dst writes to a staging path; src holds the replica down
VERIFY → dst daemon hashes the received tree and reports back; src purges its copy only after VERIFY succeeds
ACTIVATE → dst daemon swaps staging→live path and starts the replica; cluster state flips ownership to dst
```

Failure of any phase rolls back to the *previous successful* phase. Critically: SHIP can be retried; ACTIVATE failure (e.g. dst replica won't come up) restores src as owner and restarts it from its still-extant data.

### Transport

Single new endpoint on the existing daemon gRPC service:

```
service Daemon {
  rpc ShipVolume(stream VolumeChunk) returns (ShipResult);
}
```

`VolumeChunk` carries `move_id`, a path relative to the volume root, file mode, and content bytes. The stream runs over the existing peer-to-peer mTLS gRPC connection that already rides the wireguard mesh — no new listener, no new keys.

Snapshot-style: walk the source directory, send each file once. No incremental rsync in v1 — the volumes we care about (pg data dir, redis dump) are O(GB) on a B2s and copy in under a minute over wg. Add deltas later when a real workload demands it.

A per-move SHA-256 manifest is built on the source as files are sent and re-derived on the dest as files land. VERIFY succeeds iff both manifests match byte-for-byte.

### Concurrency

The leader issues at most **one** active move per `(deployment, service)` and at most **N** active moves per source node (default N=1, configurable). Moves are queued in cluster state; a backed-up queue is a normal operator-visible condition (`jaco status` shows it), not an error.

## Component changes

- `internal/runtime/volumes/` — new package. Owns the on-host volume directory layout, bind-mount construction, snapshot iterator, manifest hasher, staging-to-live swap. Pure local-FS code; no networking.
- `internal/daemon/grpc/ship.go` — implements `ShipVolume`. Streams in chunks, writes to staging under `/var/lib/jaco/volumes/<deployment>/<service>/<volume>/.staging-<move_id>/`, returns the manifest hash on stream end.
- `internal/scheduler/migration/` — new package. Drives the 5-phase state machine. One goroutine per active move on the leader; commits each phase transition through raft so a failover resumes the in-flight move from the last persisted phase.
- `internal/controlplane/fsm/` — extend the FSM with `Move{Id, Deployment, Service, Volume, Src, Dst, Phase, ManifestHash}` entries. Apply is a pure state update; the side-effects (calling `ShipVolume`, starting the replica) happen in the migration goroutine watching state.
- `internal/runtime/lifecycle/config.go` — when a service has a named volume now under JACO's management, the container build uses a bind mount to the managed path instead of `Mount{Type: Volume, ...}`. Backwards compatibility: existing deployments using plain `volumes:` continue to work with anonymous local docker volumes until they're explicitly migrated (one-time migration on next replica restart, gated by a daemon-side feature flag for the first release).

## Acceptance

- The bench `pg-primary`/`pg-replica` services in `tests/samples/jaco/jaco.yaml` drop their `placement: hosts` pins, take a planned move via `jaco node drain`, and come up on the destination with the prior database intact (rowcount preserved, replication catches up without a re-sync).
- A move interrupted by killing the leader between SHIP and VERIFY resumes on the new leader and reaches ACTIVATE without operator intervention.
- A move whose dst replica won't start (e.g. wrong CPU arch image) rolls back; src replica resumes serving from its still-resident data.
- `jaco status` surfaces per-move phase + bytes-shipped/total + ETA; the audit log records each phase transition.

## Out of scope (v1)

- **Hard-failed source node** — if src is gone before SHIP completes, the move fails permanently and durability falls back to app-level replication (pg streaming, redis replica) or external storage. Document this as the explicit non-goal it is. A future "rebuild from peer" path is conceivable for engines with quorum replication, but it's a different design.
- **Incremental / live migration** — stop-ship-start has bounded downtime (seconds for redis, tens of seconds for pg-sized volumes on B2s). Live migration requires app-aware coordination and is a follow-up.
- **Shared / CSI-style storage** — the volumes package is structured so an alternate backend (detach/attach instead of ship) can implement the same `Migrator` interface later. Not built in v1.
- **Cross-cluster migration.**

## Sequencing

This is a prerequisite for #92 (pressure-based scheduling moving stateful replicas). #92's design references this issue and gates its stateful path on this landing.

## Estimated size

Substantial — new package (`volumes`), new package (`migration`), new gRPC endpoint, FSM extension, lifecycle bind-mount rework, plus bench manifest changes and a multi-node integration test on the bed. Realistic ballpark: 2–3 medium PRs.

- PR 1 — `internal/runtime/volumes/` + lifecycle bind-mount rework + feature flag; existing deployments unchanged behavior; unit tests for the snapshot/manifest/swap.
- PR 2 — `ShipVolume` gRPC + integration test over loopback between two daemons.
- PR 3 — `migration` state machine + FSM entries + `jaco status` surface + the bench-unpinning E2E on the 3-node bed.
