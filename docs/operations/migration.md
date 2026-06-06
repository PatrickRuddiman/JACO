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

### How JACO names volumes

For a service mount like `pgdata:/var/lib/postgresql/data`:

| | volume name actually used |
|---|---|
| `docker compose up` | `<project>_pgdata` (project defaults to the compose file's directory name) |
| `jaco apply` | `jaco_<deployment>_pgdata` (the deployment name from `jaco.yaml`) |

JACO scopes every declared named volume to the deployment so two
stacks that happen to use the same bare key (`pgdata`, `data`,
`logs`, `cache`, …) cannot collide on a shared local docker volume
([`internal/runtime/compose/spec.go`](../../internal/runtime/compose/spec.go)).
The scheme matches the existing per-deployment convention used for
networks (`jaco_<deployment>_<network>`) and container names
(`<deployment>-<service>-<index>`). The prefix never appears inside
the container — the service still reaches the volume at its declared
mount path.

### Sharing a volume across stacks

When you _want_ two deployments to share storage (or you're migrating
from a stack whose volume is already named `myproject_pgdata` and you
want to keep using it in place), set the top-level `volumes.<key>.name:`
to the literal docker volume name. JACO honors it verbatim — no
deployment prefix is applied:

```yaml
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
    name: ops-shared-pgdata        # used as-is, unprefixed
```

The same escape hatch covers `external: true` — compose's "this volume
already exists, don't manage it" contract — which JACO recognises and
also leaves unprefixed.

`driver:` and `driver_opts:` on the top-level entry are still
**silently dropped**. A volume backed by an NFS or cloud driver becomes
a plain `local`-driver volume on each node. If your current stack gets
shared storage through a volume **driver**, that does not carry over —
flatten it to a plain named volume plus an explicit data copy, or
front it with application-level replication.

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

### Or copy the raw volume into the destination volume

The source volume on the old host is `<project>_pgdata`. The
destination depends on whether you let JACO scope the volume to its
deployment (default — recommended) or pin the literal name via the
`volumes.<key>.name:` escape hatch.

Default (deployment-scoped): the destination volume on the cluster
node is `jaco_<deployment>_pgdata`. Substitute the deployment name
from your `jaco.yaml` (e.g. `myapp` → `jaco_myapp_pgdata`).

```sh
# On the OLD host — confirm the real name, then export the live volume.
docker volume ls | grep pgdata
docker run --rm -v <project>_pgdata:/from:ro -v "$PWD":/backup \
  alpine tar czf /backup/pgdata.tgz -C /from .

# Copy to the node you pinned the service to.
scp pgdata.tgz node-2:/tmp/

# On node-2 — create the deployment-scoped volume JACO will mount, then load it.
docker volume create jaco_myapp_pgdata
docker run --rm -v jaco_myapp_pgdata:/to -v /tmp:/backup:ro \
  alpine sh -c 'cd /to && tar xzf /backup/pgdata.tgz'
```

If you'd rather keep using the volume name your old stack created
(e.g. you've already taken a snapshot named `myproject_pgdata` and
want JACO to mount it in place), set
`volumes: { pgdata: { name: myproject_pgdata } }` in the compose file.
JACO uses that literal verbatim and skips the deployment prefix.

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
| Volume name prefix | replaced — JACO uses `jaco_<deployment>_<key>` (not `<project>_<key>`) |
| Top-level `volumes:` `name:` / `external:` | honored as the unprefixed opt-out (compose-portable escape hatch) |
| Top-level `volumes:` `driver:` / `driver_opts:` | dropped; every volume becomes a plain `local`-driver volume on each node |
| Volume data across nodes | not replicated; pin stateful services, move data manually |
| Bind mount to a missing host path | not rejected; an empty directory is auto-created |
| `build:` | ignored — JACO pulls images, never builds |
| `restart:` | ignored — the scheduler owns restart |
| `deploy.replicas` / `deploy.placement` | ignored — set these in `jaco.yaml` |
| Host ports `80` / `443` | rejected (`reserved_port`); use `routes:` |

## Legacy v1/v2 spellings

If you are porting from a compose file written against the v1 or v2
spec, a handful of keys were dropped from the modern spec and JACO
rejects them at parse time with a typed `legacy_compose_field` error
naming the modern equivalent (issue #122). The error's
`details.field` and `details.modern_equivalent` give an actionable
diagnostic instead of an opaque "unknown field":

| legacy key | rewrite to |
|---|---|
| `log_driver: json-file` | `logging:`<br>&nbsp;&nbsp;`driver: json-file` |
| `log_opt: {max-size: 10m}` | `logging:`<br>&nbsp;&nbsp;`options:`<br>&nbsp;&nbsp;&nbsp;&nbsp;`max-size: 10m` |
| `net: host` | `network_mode: host` |
| `volume_driver: local` | use the long-form `volumes:` entry with `driver: local` (see compose spec) |
| top-level service `dockerfile:` | `build:`<br>&nbsp;&nbsp;`dockerfile: …` (then drop it — JACO ignores `build:`) |

Genuine typos (a misspelled key not in this list) keep the generic
`compose load:` wrap so they aren't misclassified.

## See also

- [`jaco.yaml` schema](../manifests/jaco-yaml.md),
  [Supported compose fields](../manifests/compose.md)
- [Scheduling](../concepts/scheduling.md) — placement, pinning, replica states
- [Getting started](../getting-started.md)
- [Backups](backups.md), [Recovery](recovery.md),
  [Troubleshooting](troubleshooting.md)
