# postgres tier — primary + streaming read replica (cross-node)

A Postgres **streaming replication** pair added to measure cross-node database
replication speed: a primary and a hot-standby read replica, pinned to
**different nodes** so the WAL stream traverses the WireGuard mesh.

- **`pg-primary`** — accepts writes; `wal_level=replica`, WAL senders enabled.
- **`pg-replica`** — `pg_basebackup`s from the primary on first start (with
  `-R`, so it boots as a streaming standby) and serves read-only queries.

Both run from the one image here (`PG_ROLE` selects the role). Persistence is
ephemeral by design — a fresh replica re-syncs from the primary, exercising the
cross-node replication path on every start. Auth is `trust` on the closed
in-cluster network (do not expose).

## Measuring replication speed

The `api` tier runs a 1 Hz heartbeat: it `UPDATE`s a timestamp row on
`pg-primary`, reads it back from `pg-replica`, and exposes the observed delay as
**`bench_pg_replica_lag_seconds`** (Prometheus, on `/metrics`). Because the
primary and replica are on different nodes and all nodes run chrony, this gauge
is the end-to-end WAL propagation time across the mesh (clock skew ≪ lag).

Cross-check directly on the primary:

```sh
psql -U bench -d bench -c \
  "SELECT client_addr, state, write_lag, flush_lag, replay_lag FROM pg_stat_replication;"
```

## Placement

`jaco.yaml` pins `pg-primary` and `pg-replica` to distinct hosts via
`placement: hosts`, guaranteeing the two live on separate nodes. Adjust the
host names there to match your cluster.
