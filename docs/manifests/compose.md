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
| `volumes`      | named volumes and host bind mounts                             |
| `ports`        | declares cluster-wide TCP listeners (see below)                |
| `depends_on`   | **ordering only**; runtime starts in topological order         |
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
