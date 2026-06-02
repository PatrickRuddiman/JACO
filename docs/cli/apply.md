---
sources:
  - cmd/jaco/apply.go
  - internal/controlplane/grpc/jaco_spec.go
  - internal/controlplane/admission/
  - internal/runtime/compose/validate.go
---

# `jaco apply`

Apply a `jaco.yaml` + compose pair to the cluster. The leader validates
both files, replicates the new `Deployment` revision through raft, and
the scheduler converges replicas to match.

## Synopsis

```
jaco apply <jaco.yaml> [--compose <path>] [--dry-run]
           [--server <host:port> --token <op>]
           [--ca-cert <path>] [--socket <path>]
```

## Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (with `--server`)       |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                                |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `--compose <path>`    | auto-detect                   | path to the compose file                      |
| `--dry-run`           | `false`                       | print the diff and exit without applying      |

When `--compose` is unset, the CLI looks for `compose.yml` then
`compose.yaml` in the same directory as `<jaco.yaml>`. If neither
exists, apply fails with `no compose file found next to <jaco.yaml>;
pass --compose explicitly`.

## Auth

Operator token (TCP) or unix-socket trust (local).

## Behavior

1. The CLI reads both files from disk and sends their raw bytes to the
   daemon via `Deploy.Apply`.
2. The daemon validates the jaco.yaml against the closed schema
   (`deployment`, `services`, `routes`; see
   [manifests/jaco-yaml.md](../manifests/jaco-yaml.md)) and the compose
   file against the supported-field allowlist (see
   [manifests/compose.md](../manifests/compose.md)). Unknown fields,
   unknown services, unknown hosts, unknown networks, a
   `cross-deployment` collision, attempts to publish a reserved port
   (80/443), and two services in the same deployment publishing the
   same host port (`port_conflict`) reject the apply with a typed
   error and **no state changes**.
3. Cross-check: every `services[*].name` in the jaco.yaml must match a
   key in the compose file. A route targeting a service that sets
   `network_mode: none` or `network_mode: service:<name>` is rejected
   — those services have no reachable listener of their own, so the
   route would publish a dead upstream.
4. **Privileged admission** (issue #119). A compose service that sets
   `privileged: true` or a non-empty `security_opt:` list requires:
   the calling operator token has `allows_privileged=true` (see
   [`jaco token issue --allow-privileged`](token.md)), AND the service
   carries `labels: { "jaco.io/allow-privileged": "true" }`. A
   missing token flag rejects with `PermissionDenied` naming the
   first offending service; a missing label rejects in step 2 with
   `validation_failed`. Admitted privileged workloads write one
   `privileged_workload_admitted` audit event per gated service.
5. The leader writes a new `Deployment{applied_revision: N+1}` through
   raft. The scheduler reconciles `ReplicaDesired` and the runtime
   converges containers.
6. The RPC returns `Applied revision: <N+1>` once the leader has
   committed the new revision. Container start + health is observed
   asynchronously; `jaco status -w` shows replicas moving through
   `pending → pulling → running`.

With `--dry-run` the apply returns the `Diff` (adds, updates, removes)
without committing. **Privileged admission still runs** under dry-run
so the diff reflects what the live apply would decide. The diff itself
currently surfaces as `No changes` on a no-op apply; richer per-entity
diffs are tracked separately.

## Exit codes

- `0` — apply succeeded, or `--dry-run` returned a diff.
- `1` — `validation_failed`, `unknown_service`, `unknown_host`,
  `unknown_network`, `reserved_port`, `port_conflict`,
  `cannot place N replicas on M pinned hosts`, `PermissionDenied`
  (privileged admission), `quorum_lost`, `no_leader`, or any auth /
  transport error.

See [Status and errors](../concepts/status-and-errors.md) for the
closed code set.

## Examples

End-to-end apply with the manifest pair side-by-side:

```sh
export JACO_TOKEN=<operator_token>
export LEADER=node-1:7000
jaco apply --server $LEADER ./hello/jaco.yaml
# Applied revision: 1
```

Dry-run on the local daemon, using an explicit compose path:

```sh
sudo jaco apply --compose ./hello/services.yml --dry-run ./hello/jaco.yaml
# No changes
```

Re-applying with a bumped image rolls one replica at a time:

```sh
# edit ./hello/docker-compose.yml: image: nginx:1.28
jaco apply --server $LEADER ./hello/jaco.yaml
jaco status --server $LEADER hello -w        # observe the rollout
```

If the apply rejects, the cluster state is unchanged. Re-edit and try
again — there is no partial-apply state to clean up.

## See also

- [`jaco status`](status.md)
- [`jaco rollback`](rollback-delete.md)
- [`jaco validate`](validate.md)
- [Manifest schema](../manifests/jaco-yaml.md)
- [Supported compose fields](../manifests/compose.md)
