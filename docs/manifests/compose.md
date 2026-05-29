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
| `env_file`     | env file(s); paths resolve against the compose file's dir      |
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
| `networks`     | per-(deployment, network) bridge attach (see below)            |

Plus the top-level `networks:` block, which declares the networks
service-level entries may reference.

## Honored-with-overrides fields

| field      | behavior                                                                |
|------------|-------------------------------------------------------------------------|
| `deploy`   | only `deploy.resources.{limits,reservations}` are read (CPU/memory cgroup limits); `replicas`, `placement`, `restart_policy`, `update_config` are parsed-but-ignored — the scheduler owns those decisions |
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

## See also

- [`jaco.yaml` schema](jaco-yaml.md)
- [Examples](examples.md)
- [Networking](../concepts/networking.md)
- [Status and errors](../concepts/status-and-errors.md)
