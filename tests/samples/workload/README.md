# workload — the orchestrator-neutral benchmark app

One application, deployed unchanged on every orchestrator. Keeping the workload
identical is what makes the comparison fair: only the orchestration layer
differs between runs, never the code or the images.

## Topology

```
            ┌────────── ingress (one route → web:80) ──────────┐
            ▼
        ┌────────┐   /api/* (nginx strips prefix)   ┌────────┐
        │  web   │ ───────────────────────────────► │  api   │  (N replicas)
        │ nginx  │                                   │ node22 │
        └────────┘                                   └───┬────┘
          static UX                          writes │       │ reads
                                                     ▼       ▼
                                          ┌───────────────┐ ┌──────────────────┐
                                          │ redis-primary │ │ redis-replica ×N  │
                                          │   (writes)    │◄│   (read-only)     │
                                          └───────────────┘ └──────────────────┘
                                                  async replication
```

- **web** — `nginx:1.27-alpine`. Serves the static UX and reverse-proxies
  `/api/*` to the `api` service (prefix stripped). Putting the path split in
  nginx means every stack needs only a single ingress route to `web:80`.
- **api** — Node 22 + TypeScript. `GET /notes` reads from a Redis replica,
  `POST /notes` writes to the Redis primary. Exposes `/healthz` and a
  Prometheus `/metrics` endpoint with HTTP latency, per-op Redis latency, and
  observed replication lag — these are the numbers the bench harness collects.
- **redis-primary / redis-replica** — see [`redis/`](redis).
- **pg-primary / pg-replica** — a Postgres streaming-replication pair pinned to
  different nodes, used to measure cross-node DB replication speed. The api
  publishes the primary→replica replay lag as `bench_pg_replica_lag_seconds`.
  See [`postgres/`](postgres). Optional: disabled when `PG_PRIMARY` is unset.

## Service contract (identical on every stack)

| service        | replicas | port | role                          |
|----------------|----------|------|-------------------------------|
| web            | 2        | 80   | ingress entry + /api proxy    |
| api            | 2        | 8080 | notes API + `/metrics`        |
| redis-primary  | 1        | 6379 | writes                        |
| redis-replica  | 2        | 6379 | reads (replicaof primary)     |
| pg-primary     | 1        | 5432 | Postgres writes (node A)      |
| pg-replica     | 1        | 5432 | streaming standby (node B)    |

Env consumed by `api`: `REDIS_PRIMARY` (default `redis-primary:6379`),
`REDIS_REPLICA` (default `redis-replica:6379`), `LISTEN_PORT` (default `8080`),
and optionally `PG_PRIMARY` / `PG_REPLICA` (+ `PG_DB`/`PG_USER`/`PG_PASSWORD`)
to enable the Postgres replication probe.

## Images

The two custom images (`web`, `api`) are built once and pushed to whatever
registry the target stack pulls from (the JACO bootstrap stands up a local
registry on node-1; see [`../jaco/bootstrap`](../jaco/bootstrap)). `redis` uses
the stock `redis:7-alpine`.

```sh
# tag for your registry, then build + push both custom images
docker build -t <registry>/bench-web:latest  web
docker build -t <registry>/bench-api:latest  api
docker push <registry>/bench-web:latest
docker push <registry>/bench-api:latest
```
