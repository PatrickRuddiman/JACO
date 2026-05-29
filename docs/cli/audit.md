# `jaco audit`

Query the cluster audit log. Each state-changing operation writes an
`AuditEvent` to raft; this command surfaces them with filters and
optional follow.

## Synopsis

```
jaco audit [--since <dur>] [--type <a,b,...>] [-f]
           [--server <host:port> --token <op>]
           [--ca-cert <path>] [--socket <path>]
           [-o table|json]
```

## Flags

| flag                  | default                       | meaning                                       |
|-----------------------|-------------------------------|-----------------------------------------------|
| `--server <addr>`     | —                             | leader gRPC; omit to use the local socket     |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer token (with `--server`)       |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                                |
| `--socket <path>`     | `/var/run/jaco/jaco.sock`     | local jacod unix socket                       |
| `--since <dur>`       | —                             | only events newer than this duration          |
| `--type <list>`       | all types                     | comma-separated audit-type filter             |
| `-f, --follow`        | `false`                       | stream new events as they arrive              |
| `-o, --output <fmt>`  | `table`                       | `table` or `json` (NDJSON when `-f`)          |

`--since` accepts any Go `time.ParseDuration` value. `--type` accepts
the lowercase short-form names: `apply`, `delete`, `rollback`,
`node_join`, `node_remove`, `token_issue`, `token_revoke`,
`certificate_issued`, `certificate_renewed`, `certificate_failed`,
`isolation_ruleset_reconciled`, `isolation_unavailable`,
`backup_taken`, `restore_completed`, `upgrade_succeeded`,
`upgrade_failed`, `rollout_invariant_hold`.

## Auth

Operator token (TCP) or unix-socket trust (local).

## Behavior

Without `-f`, the call streams the historical window matching the
filters and exits when the server closes the stream. With `-f`, the
stream stays open and new events print as they're committed.

`-o table` renders one fixed-width line per event:
`TYPE  TS  IDENTITY  PAYLOAD` where payload is `key=value …`.

`-o json` collects events into a JSON array (non-follow) or emits NDJSON
(one object per line, follow mode). `-o yaml` is **not implemented**;
the call returns `-o yaml not implemented yet` if requested.

## Exit codes

- `0` — stream completed or follow loop cancelled.
- `1` — unknown `--type`, auth, or transport error.

## Examples

Last hour of every type:

```sh
jaco audit --server $LEADER --since 1h
```

Live tail of token + apply operations:

```sh
jaco audit --server $LEADER --type apply,token_issue,token_revoke -f
```

NDJSON to file:

```sh
jaco audit --server $LEADER -o json -f > audit.ndjson
```

## See also

- [Status and errors](../concepts/status-and-errors.md)
- [Auth and tokens](../concepts/auth-and-tokens.md)
- [Observability](../concepts/observability.md)
