---
sources:
  - internal/runtime/compose/
  - internal/runtime/lifecycle/
  - internal/runtime/volumes/
  - internal/controlplane/grpc/jaco_spec.go
---

# Migrating a docker-compose stack to a JACO cluster

This guide takes a stack you run today with `docker compose up` on a
single host — including mounted volumes — and moves it onto a multi-node
JACO cluster.

JACO consumes the **same `docker-compose.yml`** plus a small
[`jaco.yaml`](../manifests/jaco-yaml.md) overlay that declares the
cluster-level concerns the single-host file never had: how many
replicas, which hosts, and public ingress. The compose file keeps
describing service shapes (image, environment, healthcheck, volumes,
networks).

The hard part of any migration is **state**. Read the next section
before you do anything else — it determines how you lay out and move
every stateful service.

## The volume reality (read this first)

Docker volumes and bind mounts are **node-local**. JACO does not
replicate volumes, has no networked/CSI storage layer, and **data does
not follow a replica** if the scheduler places it on a different node.
This has hard consequences:

- A **stateful** service must be `replicas: 1` and pinned to one node
  with `placement: hosts` (see [Scheduling](../concepts/scheduling.md)).
- If the pinned node is down, the service reports `pending` and is
  **not** rescheduled onto an empty volume elsewhere — safe for your
  data, but it means there is no automatic failover of the data itself.
- High availability for stateful data is done at the **application
  layer** (e.g. Postgres streaming replication, Redis replication),
  with each instance pinned to a different node. JACO supplies the
  cross-node network and DNS; the database does the replication.

### How JACO names volumes (it differs from `docker compose`)

For a service mount like `pgdata:/var/lib/postgresql/data`:

| | volume name actually used |
|---|---|
| `docker compose up` | `<project>_pgdata` (project defaults to the compose file's directory name) |
| `jaco apply` | `pgdata` (the bare key, no prefix) |

JACO loads the compose file under a fixed project name and lets
compose-go compute the prefixed name, but then **uses the bare
service-level key as the docker volume name** and discards the prefix
([`internal/runtime/compose/spec.go`](../../internal/runtime/compose/spec.go)
→ [`internal/runtime/lifecycle/config.go`](../../internal/runtime/lifecycle/config.go)).
When you copy data, the destination volume name is the bare key.

### The top-level `volumes:` block is ignored

JACO reads only each service's mount list, never the top-level
`volumes:` map. Everything you configure there is silently inert:

- `name:` — ignored; JACO uses the short key.
- `external: true` — the "must pre-exist, don't create" contract is
  **not** honored; JACO auto-creates the volume anyway.
- `driver:` / `driver_opts:` — **dropped**. A volume backed by an NFS
  or cloud driver becomes a plain `local`-driver volume on each node.

If your current stack gets shared storage through a volume **driver**,
that does not carry over — flatten it to a plain named volume plus an
explicit data copy, or front it with application-level replication.

### Bind mounts are not preflighted

A bind mount whose host path does not exist on the target node is **not
rejected** at apply — docker auto-creates an empty directory there. A
bind-mounted data directory silently comes up **empty** on the cluster
node unless you pre-stage the path first.

## Step 1 — Inventory and classify

List every service and label each one:

- **Stateless** (web, API, workers, proxies) — no meaningful local
  state. These become multi-replica, spread across the cluster.
- **Stateful** (databases, caches with persistence, queues, anything
  whose volume holds data you can't lose) — single pinned replica, or
  app-level replication.

For every `volumes:` entry, decide:

- **Real persistent data** (a database data directory) → must be moved
  (Step 5) and the service pinned (Step 4).
- **Config / source bind mounts** (`./nginx.conf:/etc/nginx/...`,
  `./src:/app`) → these host paths won't exist on cluster nodes. Bake
  config into the image, or deliver it via `environment` / `env_file`.

## Step 2 — Stand up the cluster

Install JACO on three hosts and form the cluster
([Getting started](../getting-started.md), [`jaco cluster`](../cli/cluster.md),
[`jaco node`](../cli/node.md)):

```sh
# node 1
sudo jaco cluster init
# Save the printed operator_token — it cannot be recovered.

export JACO_TOKEN=<operator_token>
jaco node issue-join-token            # prints the join command

# nodes 2 and 3
sudo jaco node join --peer <node-1-host>:7000 --token <single-use>
```

Confirm all three are `ready`:

```sh
export LEADER=<node-1-host>:7000
jaco node list --server $LEADER
```

## Step 3 — Get images into a registry

JACO **pulls** every image and **never builds** — the compose `build:`
field is accepted but ignored
([compose.md](../manifests/compose.md)). Any image you build locally
must be pushed to a registry the cluster nodes can reach, and the
compose `image:` must point at it. Plan registry credentials/network so
each node can pull.

## Step 4 — Author the manifests

### Trim the compose file

Keep the honored fields; remove anything outside the allowlist (unknown
service fields reject the apply with `validation_failed`). Two specific
edits almost every stack needs:

- **Reserved ports** — remove any `ports:` entry publishing host port
  `80` or `443`; those belong to JACO's ingress and reject with
  `reserved_port`. Public HTTP(S) moves to `jaco.yaml` `routes:`.
- **Other published ports** — `"6379:6379"` becomes a cluster-wide
  raw-TCP listener automatically; no change needed.

`restart:`, `build:`, and `deploy.replicas/placement` are
parsed-but-ignored — the scheduler owns those decisions. `depends_on`
is honored as **start ordering only**.

### Write the `jaco.yaml` overlay

Declare replicas, placement, and routes. Stateless services spread;
stateful services pin to one node.

Before (single-host `docker-compose.yml`, abridged):

```yaml
services:
  web:
    image: myorg/web:1.4
    ports: ["80:80", "443:443"]
    depends_on: [api]
  api:
    image: myorg/api:1.4
    environment:
      DATABASE_URL: postgres://app@db:5432/app
      REDIS_URL: redis://cache:6379
    restart: always
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
    restart: always
  cache:
    image: redis:7
volumes:
  pgdata:
    driver: local        # ignored by JACO — see "volume reality"
```

After — compose (drop the reserved-port publish on `web`; everything
else stays):

```yaml
services:
  web:
    image: myorg/web:1.4
    depends_on: [api]
  api:
    image: myorg/api:1.4
    environment:
      DATABASE_URL: postgres://app@db:5432/app
      REDIS_URL: redis://cache:6379
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
  cache:
    image: redis:7
```

After — `jaco.yaml` overlay:

```yaml
deployment: myapp
services:
  - name: web
    replicas: 3            # stateless → spread across all 3 nodes
  - name: api
    replicas: 3
  - name: db
    replicas: 1            # stateful → single instance...
    placement: hosts
    hosts: [node-2]        # ...pinned to the node holding its volume
  - name: cache
    replicas: 1            # in-memory cache → single instance is fine
routes:
  - domain: app.example.com
    service: web
    port: 80
    tls: auto
```

Service names in `jaco.yaml` must match compose service keys exactly, or
the apply rejects with `unknown_service`.

### Validate offline

You can validate both files without touching the cluster
([`jaco validate`](../cli/validate.md)):

```sh
jaco validate --jaco ./jaco.yaml --compose ./docker-compose.yml
```

## Step 5 — Move the data

For each stateful service, pick its target node (the one named in
`hosts:`) and stage its data there **before** apply.

### Prefer application-native dump/restore

For databases this is the safest path — it avoids uid, page-format, and
engine-version mismatches that raw volume copies hit:

```sh
# On the old host
docker exec <old-db-container> pg_dumpall -U postgres > dump.sql

# After db comes up on its pinned node (Step 6), load it
psql "postgres://postgres@<node-2-host>:5432/" < dump.sql
```

### Or copy the raw volume into the bare-named volume

Remember the name change: the source is `<project>_pgdata`, the
destination is the bare `pgdata`.

```sh
# On the OLD host — confirm the real name, then export the live volume.
docker volume ls | grep pgdata
docker run --rm -v <project>_pgdata:/from:ro -v "$PWD":/backup \
  alpine tar czf /backup/pgdata.tgz -C /from .

# Copy to the node you pinned the service to.
scp pgdata.tgz node-2:/tmp/

# On node-2 — create the bare-named volume JACO will mount, then load it.
docker volume create pgdata
docker run --rm -v pgdata:/to -v /tmp:/backup:ro \
  alpine sh -c 'cd /to && tar xzf /backup/pgdata.tgz'
```

### Bind mounts

Create and populate the exact host path on the pinned node before apply
— otherwise the service starts against an empty auto-created directory.

## Step 6 — Cut over

1. **Quiesce writes** on the old stack — stop the app, or put the
   database in read-only — so no new data lands after your last sync.
2. **Final data sync** — re-run the dump/restore or volume copy to
   capture anything written since Step 5.
3. **Apply** ([`jaco apply`](../cli/apply.md)):
   ```sh
   jaco apply --server $LEADER ./jaco.yaml --compose ./docker-compose.yml
   ```
   Dry-run first with `--dry-run` to print the diff without applying.
4. **Watch convergence** ([`jaco status`](../cli/status.md)):
   ```sh
   jaco status --server $LEADER myapp -w
   ```
   The pinned `db` should land on `node-2` and reach `running`; stateless
   services spread to `running` on all three nodes. A `db` stuck in
   `pending` with `cannot_satisfy_host_placement` means `node-2` isn't
   eligible (check `jaco node list`).
5. **Verify** — check routes/TLS resolve, tail logs
   ([`jaco logs`](../cli/logs.md)), and confirm data integrity in the new
   stack:
   ```sh
   jaco logs --server $LEADER myapp/db --follow
   ```
6. **Decommission** the old single-host stack only after you've
   confirmed data and traffic on the cluster.

## Step 7 — Harden stateful tiers (optional)

A single pinned instance is a single point of failure: if its node
dies, the service sits in `pending` until the node returns (data can't
follow). For real HA, run application-level replication with each
instance pinned to a different node — the pattern the shipped sample
uses ([`tests/samples/jaco/`](../../tests/samples/jaco)):

```yaml
services:
  - name: pg-primary
    replicas: 1
    placement: hosts
    hosts: [node-2]
  - name: pg-replica
    replicas: 1
    placement: hosts
    hosts: [node-3]
```

The replica streams WAL from the primary across the WireGuard mesh.
JACO keeps each instance on its node and its volume; the database owns
the replication and failover policy.

## What does not carry over

| compose feature | behavior under JACO |
|---|---|
| Volume name prefix | none — JACO uses the bare key (`pgdata`, not `<project>_pgdata`) |
| Top-level `volumes:` `name:` / `external:` / `driver:` / `driver_opts:` | ignored; plain `local` volume named by the short key |
| Volume data across nodes | not replicated; pin stateful services, move data manually |
| Bind mount to a missing host path | not rejected; an empty directory is auto-created |
| `build:` | ignored — JACO pulls images, never builds |
| `restart:` | ignored — the scheduler owns restart |
| `deploy.replicas` / `deploy.placement` | ignored — set these in `jaco.yaml` |
| Host ports `80` / `443` | rejected (`reserved_port`); use `routes:` |

## See also

- [`jaco.yaml` schema](../manifests/jaco-yaml.md),
  [Supported compose fields](../manifests/compose.md)
- [Scheduling](../concepts/scheduling.md) — placement, pinning, replica states
- [Getting started](../getting-started.md)
- [Backups](backups.md), [Recovery](recovery.md),
  [Troubleshooting](troubleshooting.md)
