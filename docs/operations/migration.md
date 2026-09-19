---
sources:
  - internal/runtime/compose/
  - internal/runtime/lifecycle/
  - internal/runtime/volumes/
  - internal/runtime/reconciler/
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
| `jaco apply` | `jaco_v2_<digest>` (cluster ID, deployment name and volume key) |

Default named volumes use a full, lowercase SHA-256 digest of the
**cluster ID, deployment and compose volume key**. Each component is
encoded as its UTF-8 byte length in decimal, a colon, then its bytes,
in that order. Length framing removes the ambiguity of joining names
with underscores. The result is always a 72-character Docker-safe name
([`internal/runtime/compose/spec.go`](../../internal/runtime/compose/spec.go)).
Names remain stable across replicas, redeploys and daemon restarts.
The persisted cluster ID survives JACO backup/restore; initializing a
new cluster creates a different identity. A control-plane backup does
**not** back up Docker volume contents.

JACO creates default volumes with `jaco.volume_identity=2`,
`jaco.cluster_id`, `jaco.deployment` and `jaco.volume_key` labels.
Every required label and the returned Docker name must match before
reuse, including when Docker returns an existing volume during a
concurrent create. Unlabelled or differently owned volumes are not
claimed automatically. The mount path inside the container is unchanged.

Find a managed volume by its labels, not by guessing its digest:

```sh
docker volume ls --filter label=jaco.volume_identity=2 \
  --filter label=jaco.deployment=myapp --filter label=jaco.volume_key=pgdata
```

Also filter `jaco.cluster_id=<cluster-id>` when multiple clusters use the
same engine. Network names are unchanged by this volume naming scheme.

### Upgrading legacy default volumes

Older JACO versions used `jaco_<deployment>_<key>`. These names are
ambiguous: `orders` + `prod_data` and `orders_prod` + `data` both name
`jaco_orders_prod_data`. An unlabelled volume, or a single remaining
container using it, is not proof of exclusive ownership.

**Plan the storage decision before upgrading a stateful deployment.**
If an owned v2 volume does not exist but the old name does, JACO reports
`PENDING / volume_migration_required` instead of creating an empty
replacement. It also refuses to switch an existing container's default
mount to a different volume, even if the v2 volume already exists.
A missing volume still referenced by an existing container must be
recovered rather than recreated empty.

These checks run before stopping, restarting or replacing that container.
Running containers remain running; stopped containers remain stopped.
The runtime cancels their periodic health watcher while blocked and reports
the migration status. `PENDING` does not trigger the automatic
health restarter. Starts and future replacements remain blocked until
the operator resolves the storage decision; this is not a live migration.
JACO never copies, renames, deletes or automatically adopts legacy data.

For deliberate **same-source adoption**:

1. On the pinned Docker host, inspect the exact volume and every current
   consumer. Verify its application data, deployment history and a usable
   backup; do not infer ownership from the old name alone.
   ```sh
   docker volume inspect jaco_orders_prod_data
   docker ps -a --filter volume=jaco_orders_prod_data \
     --format '{{.ID}} {{.Names}} {{.Status}}'
   ```
2. If colliding workloads already shared data, quiesce them and plan
   application-specific recovery or separation. New names cannot unmix
   existing data. Do not automatically pin both deployments to the old
   volume unless that sharing is now intentional.
3. Pin the service to the host with the verified data. Set the exact
   existing name and require its presence:
   ```yaml
   volumes:
     prod_data:
       name: jaco_orders_prod_data
       external: true
   ```
4. Apply the manifest and inspect `jaco status`. The runtime retries on
   its normal reconcile/safety tick (up to 30 seconds, then health polling).
   A running container already using this same source can be retained
   without a roll; no ownership labels or data are changed.

`external: true` is important: if the volume is absent on the selected
engine, JACO reports `external_volume_missing` instead of creating empty
storage. Do not remove that flag just to bypass a missing-data error.
Changing to a **different** source is a separate, deliberate data migration;
top-level volume edits alone do not force a container roll. Do not delete
a production deployment merely to change its volume name.

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
already exists, don't manage it" contract. JACO checks that the exact
volume exists on the engine and leaves its name, driver and labels alone.
Without an explicit `name:`, an external volume uses its compose key.
An explicit `name:` **without** `external: true` still permits Docker to
create the volume if absent; use `external: true` whenever existing data
is required.

`driver:` and `driver_opts:` on the top-level entry are still
**silently dropped for newly created volumes**. A request for a new
volume backed by an NFS or cloud driver becomes
a plain `local`-driver volume on each node. If your current stack gets
shared storage through a volume **driver**, that does not carry over —
pre-provision and verify it as external, flatten it to a plain named
volume plus an explicit data copy, or front it with application-level
replication.

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
  config into the image, deliver it via `environment` / `env_file`, or
  hoist per-stack values into a top-level
  [`environment: <path>`](../manifests/jaco-yaml.md#environment) on
  `jaco.yaml` and reference them via `${VAR}` in the compose file —
  see Step 4 below for the migration shape.

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

### Hoist per-stack values into `environment:` (optional)

Compose `.env` files at the project root are not honored by JACO
(the daemon never reads operator-side files). The equivalent shape
is the top-level [`environment: <path>`](../manifests/jaco-yaml.md#environment)
on `jaco.yaml`, loaded CLI-side and used as the `${VAR}`
interpolation source for the whole compose document.

If your single-host stack relied on a project `.env`:

```sh
# single-host today
REGISTRY=ghcr.io DB_URL=postgres://… docker compose up
# or implicitly via ./.env
```

rename the file and point `jaco.yaml` at it explicitly:

```yaml
# jaco.yaml
deployment: myapp
environment: .env            # path relative to this jaco.yaml
routes: ...
services: ...
```

```yaml
# docker-compose.yml
services:
  api:
    image: ${REGISTRY}/myorg/api:1
    environment:
      DB_URL: ${DB_URL}
```

`${VAR}` references resolve from the env file the CLI loads —
process-environment passthrough is deliberately NOT honored
(manifests stay explicit and reproducible across operators / hosts).
Service-level `env_file:` keeps working in parallel; per
compose-spec precedence, the explicit `environment:` value on a
service wins over a service-level `env_file:` entry for matching
keys.

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

The source volume on the old host is `<project>_pgdata`. For pre-seeding
data, choose an explicit destination name and use `external: true`.
Do not guess a generated v2 name or forge JACO ownership labels.
The following example uses `myapp-imported-pgdata`; verify that this
destination name is unused before creating it. Never restore over an
existing volume without a separate backup and an intentional recovery plan.
Raw database copies require an application-consistent, quiesced source.

```sh
# On the OLD host — inspect the exact name before exporting quiesced data.
docker volume inspect <project>_pgdata &&
docker run --rm -v <project>_pgdata:/from:ro -v "$PWD":/backup \
  alpine tar czf /backup/pgdata.tgz -C /from .

# Copy to the node you pinned the service to.
scp pgdata.tgz node-2:/tmp/

# On node-2 — deliberately create the verified-unused destination, then load it.
docker volume create myapp-imported-pgdata
docker run --rm -v myapp-imported-pgdata:/to -v /tmp:/backup:ro \
  alpine sh -c 'cd /to && tar xzf /backup/pgdata.tgz'
```

Verify the restored data before apply, then declare:

```yaml
volumes:
  pgdata:
    name: myapp-imported-pgdata
    external: true
```

To retain verified existing data in place instead, use its exact old
name (for example `myproject_pgdata`) with `external: true`. JACO uses
that literal verbatim, without creating or claiming a replacement.

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
| Default volume identity | `jaco_v2_<digest>` over cluster/deployment/key, with ownership validation; ambiguous legacy names require deliberate adoption |
| Top-level `volumes:` `name:` / `external:` | honored as the unprefixed opt-out; external volumes must already exist on the selected engine |
| Top-level `volumes:` `driver:` / `driver_opts:` | dropped when creating volumes; pre-existing external volumes retain their configuration |
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
