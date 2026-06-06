---
sources:
  - internal/runtime/compose/
  - internal/runtime/lifecycle/config.go
---

# Supported compose fields

JACO consumes a standard `docker-compose.yml` v3+ file. The supported
service-level fields are a closed allowlist; anything else rejects the
apply with `validation_failed` and the offending field name. The
authoritative list lives in
[`internal/runtime/compose/validate.go`](../../internal/runtime/compose/validate.go).

This contract is what lets the same compose file run under
`docker compose up` for a local dry-run **and** under `jaco apply` for
production, without two parallel definitions.

## Honored service fields

These fields are parsed and passed through to docker on container
create:

| field          | notes                                                          |
|----------------|----------------------------------------------------------------|
| `image`        | pulled with exponential backoff (1s → 2s → … → 1h cap)         |
| `command`      | overrides image CMD                                            |
| `entrypoint`   | overrides image ENTRYPOINT                                     |
| `environment`  | env vars                                                       |
| `env_file`     | env file(s); merged into `environment` **client-side** before apply (see [§ `env_file` resolution](#env_file-resolution)) |
| `volumes`      | named volumes and host bind mounts. Named volumes are scoped per-deployment as `jaco_<deployment>_<key>`; top-level `volumes.<key>.name:` (or `external: true`) is the compose-portable escape hatch for sharing storage across stacks. See [Migration → How JACO names volumes](../operations/migration.md#how-jaco-names-volumes) |
| `ports`        | declares cluster-wide TCP listeners (see below)                |
| `depends_on`   | **ordering only**; runtime starts in topological order. Closed condition enum: `service_started` (compose default) and `service_healthy`. `service_completed_successfully` is rejected — JACO does not model run-to-completion services. Self-deps and cross-deployment refs are rejected. Dependencies are evaluated **cluster-wide** (any replica of the dep service in the satisfying state unblocks the dependent, even on a different host). See [§ `depends_on` semantics](#depends_on-semantics) |
| `healthcheck`  | docker built-in healthcheck; drives JACO replica state         |
| `labels`       | merged with JACO-managed labels (see "Labels JACO adds")       |
| `user`         | UID/GID for the container process                              |
| `working_dir`  | container CWD                                                  |
| `tmpfs`        | tmpfs mount(s)                                                 |
| `cap_add`      | added Linux capabilities                                       |
| `cap_drop`     | dropped Linux capabilities                                     |
| `sysctls`      | sysctl key/value pairs                                         |
| `ulimits`      | resource ulimits                                               |
| `read_only`    | read-only root filesystem                                      |
| `networks`     | per-service network attach; the jaco overlay may override (see [`jaco.yaml`](jaco-yaml.md#networks)) |
| `logging`      | modern compose `logging:` block (`driver` + `options`); projected onto docker's container log config. Nil/absent uses docker's default driver. Legacy top-level `log_driver`/`log_opt` are rejected by the compose loader |
| `stop_signal`  | signal sent on container stop (compose default SIGTERM). Persisted on the container so `docker stop` and `jaco rm` both honor it |
| `stop_grace_period` | seconds to wait between `stop_signal` and SIGKILL. Persisted on the container; pre-issue #114 every service used a hardcoded 10s |
| `hostname`, `domainname` | container's `/etc/hostname` and DNS domain |
| `extra_hosts` | `[host:ip, …]` appended to `/etc/hosts` inside the container |
| `dns`, `dns_search`, `dns_opt` | per-container DNS overrides. An explicit `dns:` list overrides JACO's per-bridge resolvers; empty falls back to JACO's DNS Manager |
| `init`         | run `tini` (docker's bundled PID 1) as init |
| `shm_size`     | size of `/dev/shm` (compose syntax: `"64m"`, `"1g"`) |
| `ipc`, `pid`, `uts`, `userns_mode`, `cgroup`, `cgroup_parent` | namespace knobs forwarded verbatim. `host`-mode values weaken isolation by design; JACO honors them as-written with no runtime gate |
| `network_mode` | closed accept-set: empty (default — attach the per-deployment bridge), `none` (no network at all), `service:<name>` (share the netns of another service in the same deployment). `host`, `bridge`, `container:<id>`, and any named-network value are **rejected** — they bypass the per-deployment bridge, the WireGuard mesh, the nftables isolation, and ingress. `service:<name>` requires the target to live on the same docker daemon (issue #121); the sidecar bounces on the first deploy waiting for its primary's running container. A service that sets `network_mode` cannot receive an ingress route (apply rejects with `validation_failed`) |
| `privileged`, `security_opt` | gated. Requires both `labels: { "jaco.io/allow-privileged": "true" }` on the service AND a calling operator token with `allows_privileged=true` ([`jaco token issue --allow-privileged`](../cli/token.md)). See [§ Privileged services](#privileged-services) |
| `devices`      | host device bind-mounts (e.g. `/dev/fuse`, `/dev/snd`, `/dev/dri`). Compose short (`"/dev/fuse:/dev/fuse:rwm"`) and long form (`{source, target, permissions}`) both honored. Grants host-kernel surface; operator-side policy gating is out of scope for this PR |
| `gpus`         | modern GPU request syntax (`gpus: all` or long-form list with `driver`/`count`/`device_ids`/`capabilities`/`options`). Forwarded onto docker `HostConfig.DeviceRequests`. Requires the operator-managed nvidia-container-runtime (or AMD equivalent) on each node |
| `pull_policy`  | per-service pull strategy. Accepted values: `always` and `missing` (current JACO behavior — call `ImagePull`, daemon manifest-checks; cheap when up-to-date), `never` (skip the pull entirely; needed for air-gapped operators that side-load images), `build` (treated as `missing` — JACO never builds). `daily`/`weekly` are rejected with `validation_failed` |

Plus the top-level `networks:` block, which declares the networks
service-level entries may reference.

## Honored-with-overrides fields

| field      | behavior                                                                |
|------------|-------------------------------------------------------------------------|
| `deploy`   | `deploy.resources.{limits,reservations}` set the per-replica CPU/memory cgroup limits. `deploy.replicas` (issue #99) supplies the **default replica count** for the service when `jaco.yaml`'s `services[*].replicas` is unset — JACO's overlay still wins when present. `deploy.placement`, `restart_policy`, and `update_config` remain parsed-but-ignored — the scheduler owns those decisions |
| `cpus`, `mem_limit`, `mem_reservation`, `pids_limit`, `cpu_shares`, `cpuset` | legacy v2 resource keys; honored as a fallback when `deploy.resources` is absent. When both are present, `deploy.resources` wins |

## Explicitly accepted-and-ignored fields

These parse but don't take effect — useful for keeping one compose file
usable under both `docker compose` and `jaco apply`:

- `restart` — the scheduler owns restart decisions cluster-wide; the
  field is dropped at container create.
- `build` — JACO never builds; images are pulled from a registry the
  runtime can reach.
- `name` — JACO overrides container names with the replica id.

## Reserved host ports

Compose services may not publish host ports `80` or `443`. Those
belong to JACO's embedded Caddy ingress; publishing them is rejected
at apply with `reserved_port`:

```
Error: reserved_port: service "X" publishes reserved host port 80 (entry "80:80");
       80 and 443 belong to JACO's HTTP/S ingress
```

HTTP/S routing for declared domains lives in `jaco.yaml`'s `routes`
block. See [`jaco-yaml.md`](jaco-yaml.md).

## `ports` semantics

A `ports:` entry that publishes a host port — e.g. `"6379:6379"` —
declares a **cluster-wide raw-TCP listener** on host port `6379`. Every
node listens on that port and forwards to a healthy replica of the
service, wherever it runs. Two deployments may not publish the same
host port (the proto's `TCPRoute` is keyed by `published_port`
cluster-wide).

Entries with no published host side (`"6379"` alone) are documentation
and silently pass.

## `env_file` resolution

`env_file:` is resolved **client-side** by `jaco apply`, before the
compose document crosses the wire. The daemon node does not have the
operator's local `.env` files on disk, so a daemon-side resolution is
impossible; instead the CLI loads each referenced file relative to the
compose file's directory, folds the contents into the service's
`environment:` map, and ships a compose document that no longer
mentions `env_file:`. The daemon rejects any compose document that
still carries `env_file:` with the stable error code
`env_file_unresolved` — an old CLI talking to a new daemon fails
loudly rather than silently dropping the variables.

Precedence follows compose-spec semantics, enforced end-to-end by
`compose-go`:

1. an explicit `environment:` value wins over anything in `env_file`;
2. when multiple `env_file:` entries declare the same key, the **later
   file wins**.

Variables with no value (`FOO:` with nothing after the colon, the
compose convention for "inherit from the process environment at apply
time") round-trip through the resolver as YAML null and reach the
runtime as `FOO=`.

Two practical consequences:

- `--compose <path>` (or auto-discovery next to `jaco.yaml`) is the
  only supported source for compose documents that use `env_file:`.
  Piping a compose document through stdin while it references
  `env_file:` is rejected up front — there is no defensible base
  directory for relative paths.
- `jaco apply --dry-run` runs the resolver before computing the diff,
  so the diff reflects the values the daemon would actually see.

## `depends_on` semantics

The runtime defers a replica's `Start` until every required dep
entry is satisfied. Conditions:

- `service_started` (compose default for the bare `depends_on: [api]`
  list form) — satisfied when **at least one** replica of the named
  service is in `running` or `degraded`. `pulling` does **not**
  satisfy: the container hasn't been run on docker yet, so starting
  the dependent would race the dep's actual `docker run`.
- `service_healthy` — satisfied when at least one replica is in
  `running`. `degraded` does **not** satisfy — a waiter chose
  `service_healthy` explicitly because it needs a healthy peer, not
  just a live one.

Evaluation is **cluster-wide, not per-host**. A web replica on
`jaco-1` with `depends_on: [api]` is unblocked the moment any `api`
replica reaches the wait condition, even when that replica lives on
`jaco-3`. This matches operator expectations from compose ("api is up
somewhere") and avoids deadlocks when the scheduler spreads dep and
dependent across different hosts.

Unsatisfied deps surface as a deferred replica — the next 30 s safety
tick (or a `ReplicasObserved` watch event for a transition INTO a
satisfying state) re-dispatches the start. Operators see "depends_on
unmet; deferring start" in the node logs.

## Privileged services

A service that sets `privileged: true` or a non-empty `security_opt:`
list trips the **two-fence admission gate** (issue #119):

1. **Schema-time** — the service MUST carry
   `labels: { "jaco.io/allow-privileged": "true" }` (exact string,
   bare booleans / `True` / `1` do **not** count — compose serialises
   label values as strings). Missing the label rejects locally via
   `jaco validate` and at the daemon on `jaco apply` with
   `validation_failed` naming the gated fields.
2. **Apply-time** — the calling operator's token MUST have
   `allows_privileged=true`. The bootstrap token does not; mint one
   with `jaco token issue --name <id> --allow-privileged`. Missing the
   flag rejects with `PermissionDenied` naming the first offending
   service.

Local unix-socket callers bypass the token check (the socket's `0660`
filesystem permissions already gate operator-class access); the label
check still runs.

Each admitted privileged service writes one
`privileged_workload_admitted` audit event after the apply commits, so
the audit log records every workload that actually landed (best-effort
— an audit failure does not fail the apply, mirroring other
post-commit audit emissions).

## Rejected fields

Anything not in the allowlist above is rejected. The closed-set is
deliberate — silent acceptance of compose fields JACO does not honor
would make production behavior differ from the operator's mental
model. The full closed set lives in
[`compose/validate.go::allowedServiceFields`](../../internal/runtime/compose/validate.go).

## Labels JACO adds

JACO merges its own labels into every container it creates. These are
the runtime's source of truth for orphan reclaim on daemon restart:

- `jaco.cluster_id`
- `jaco.deployment`
- `jaco.service`
- `jaco.replica_id`
- `jaco.replica_index`
- `jaco.raft_index`

Compose `labels:` are merged on top; conflicts with JACO-managed keys
are overridden by JACO.

## Validation flow on apply

1. Parse jaco.yaml; reject unknown top-level or service-level keys.
2. Parse compose YAML; reject service-level fields outside the
   allowlist (`validation_failed`) and unknown network references
   (`unknown_network`).
3. Reject any `ports:` entry publishing 80 or 443 (`reserved_port`).
4. Cross-check every `services[*].name` in the jaco manifest exists in
   the compose file.
5. Cross-check every `hosts[*]` is a known cluster member
   (`unknown_host`).

You can run this validation locally without the cluster:

```sh
jaco validate --jaco ./hello/jaco.yaml --compose ./hello/docker-compose.yml
```

See [`jaco validate`](../cli/validate.md).

## Legacy v1/v2 spellings

A handful of v1/v2-era compose keys were dropped from the modern
spec. JACO rejects them at parse time with a typed
`legacy_compose_field` error naming the modern equivalent, so a
migration from an older compose file produces an actionable diagnostic
instead of an opaque "unknown field":

| legacy key | modern equivalent |
|---|---|
| `log_driver` | `logging.driver` |
| `log_opt` | `logging.options` |
| `net` | `network_mode` |
| `volume_driver` | no direct equivalent; use long-form `volumes:` with `driver:` |
| `dockerfile` (top-level service key) | `build.dockerfile` |

## See also

- [`jaco.yaml` schema](jaco-yaml.md)
- [Examples](examples.md)
- [Migration](../operations/migration.md) — porting an existing
  compose stack, including how JACO names and handles volumes
- [Networking](../concepts/networking.md)
- [Status and errors](../concepts/status-and-errors.md)
