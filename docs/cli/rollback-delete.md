# `jaco rollback` and `jaco delete`

State-changing deployment commands. Both currently require `--server`
and an operator bearer token; the unix-socket path is not wired for
these two RPCs yet.

## `jaco rollback`

### Synopsis

```
jaco rollback <deployment> --server <host:port> [--token <op>] [--ca-cert <path>]
```

### Flags

| flag                  | default                       | meaning                                  |
|-----------------------|-------------------------------|------------------------------------------|
| `--server <addr>`     | — (required)                  | leader gRPC                              |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token                    |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                           |

### Auth

Operator token, required.

### Behavior

Reverts `<deployment>` to its `previous_revision`. The scheduler rolls
replicas back one at a time using the prior `jaco.yaml` + compose
pair the cluster already has on hand. Routes and certs revert in step.

The previous revision must exist — there is no rollback past the first
applied revision. On a freshly-applied deployment the call returns the
new revision number.

### Exit codes

- `0` — rolled back; the new active revision is printed.
- `1` — `validation_failed` (no previous revision), `no_leader`, or
  auth / transport error.

### Examples

```sh
jaco rollback --server $LEADER hello
# Rolled back to revision: 3
```

## `jaco delete`

### Synopsis

```
jaco delete <deployment> --server <host:port> [--token <op>] [--ca-cert <path>]
```

### Flags

Same shape as `rollback`.

### Auth

Operator token, required.

### Behavior

Cascades: stops + removes every replica of the deployment, removes its
ingress routes from the Caddy config, drops the per-(deployment,
network) bridges as their last replicas disappear, and releases the
subnet allocations back into the IPAM pool. Managed TLS certs for the
deployment's domains stop renewing and are deleted.

The call returns once raft has committed the delete; container teardown
proceeds asynchronously, observable via `jaco status -w`.

### Exit codes

- `0` — delete committed.
- `1` — auth or transport error.

### Examples

```sh
jaco delete --server $LEADER hello
# Deleted deployment: hello
```

## See also

- [`jaco apply`](apply.md)
- [`jaco status`](status.md)
- [Scheduling](../concepts/scheduling.md)
