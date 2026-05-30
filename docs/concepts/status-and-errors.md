---
sources:
  - internal/controlplane/grpc/
  - internal/runtime/compose/
  - proto/jaco/v1/entities.proto
---

# Status and errors

Every gRPC handler returns a typed envelope:

```proto
message Error {
  string code = 1;
  string message = 2;
  map<string, string> details = 3;
}
```

`code` is from a **closed set** — the protobuf field is a string so
future codes can be added forward-compatibly, but the documentation
below is the contract; clients SHOULD branch on `code` and surface
`message + details` verbatim.

CLI renders these as `Error: <code>: <message>` on stderr and exits
non-zero. See
[`internal/controlplane/grpc`](../../internal/controlplane/grpc) and
[`internal/runtime/compose`](../../internal/runtime/compose) for the
constructors.

## Error codes (closed set)

| code                                  | when                                                                                  |
|---------------------------------------|---------------------------------------------------------------------------------------|
| `cluster_uninitialized`               | RPC arrived before `Cluster.Init` / `Cluster.Join` completed                          |
| `cluster_already_initialized`         | `Cluster.Init` on a node that already has raft state                                  |
| `node_already_member`                 | `Cluster.Join` against an already-joined node, or hostname collision                  |
| `no_leader`                           | leader unreachable; election in progress; client may retry                            |
| `quorum_lost`                         | minority side of a partition; writes rejected                                         |
| `token_invalid`                       | bearer token does not match any known operator token                                  |
| `token_revoked`                       | matching token, but `revoked_at` is set                                               |
| `validation_failed`                   | manifest schema violation; details include `field`                                    |
| `unknown_service`                     | jaco service not present in compose                                                   |
| `unknown_host`                        | `hosts[*]` not a cluster member                                                       |
| `unknown_network`                     | service-level network not in top-level `networks:`                                    |
| `reserved_port`                       | compose service publishes 80 or 443                                                   |
| `replicas_exceed_pinned_hosts`        | `placement: hosts` with too few hosts for the requested replicas                      |
| `image_pull_failed`                   | runtime gave up after retries (still emits backoff state per attempt)                 |
| `cert_failed`                         | ACME issuance failed past retry budget                                                |
| `docker_error`                        | docker daemon refused or errored (disk full, daemon stopped, etc.)                    |
| `isolation_unavailable`               | node could not bring up nftables ruleset; replicas not scheduled here                 |
| `isolation_self_test_failed`          | startup self-test of nftables ruleset failed                                          |
| `subnet_pool_exhausted`               | IPAM pool ran out of `/24`s                                                           |
| `upgrade_verification_failed`         | minisign signature or SHA-256 checksum mismatch in `self-upgrade`                     |
| `upgrade_failed`                      | post-upgrade health check failed; rollback executed                                   |
| `internal`                            | unrecoverable daemon error not better-categorized; details include `reason`           |

`message` is human-readable; `details` is a flat string-to-string map
of structured context (e.g. `{service: web, field: replicas}` for a
validation failure, `{attempt: 7, next_retry_at: …}` for an image-pull
backoff).

## Replica states (closed set)

`ReplicaObserved.state` from
[`proto/jaco/v1/entities.proto`](../../proto/jaco/v1/entities.proto):

| state       | meaning                                                                                       |
|-------------|-----------------------------------------------------------------------------------------------|
| `pending`   | `ReplicaDesired` received; image not yet pulled. A failing pull stays here with `code: image_pull_failed` (and the error in `details.reason`) while it retries |
| `pulling`   | image pull in progress (reported on the first attempt)                                        |
| `running`   | container up; first `healthy` healthcheck observed (or `running + no healthcheck` for 5 s)   |
| `degraded`  | `State.Health.Status = unhealthy` observed                                                    |
| `updating`  | set by the scheduler during a rolling update; runtime reads but doesn't write                 |
| `failed`    | terminal error: `image_pull_failed`, `docker_error`, `restart_exhausted` (scheduler-driven)   |
| `stopped`   | replica removed from desired set; container stopped + removed                                 |

A `failed` replica is not retried automatically beyond the
3-consecutive-restart budget; it requires a fresh `Deploy.Apply`
(which increments revision and resets state).

## Deployment status

`DeploymentStatus`:

| status     | meaning                                                                                  |
|------------|------------------------------------------------------------------------------------------|
| `pending`  | scheduling cannot proceed; `status_details` carries `reason` and supporting fields       |
| `active`   | every desired replica has converged                                                      |

## Node status

`NodeStatus`:

| status                    | meaning                                                                                                   |
|---------------------------|-----------------------------------------------------------------------------------------------------------|
| `joining`                 | post-`Cluster.Join`, pre-isolation-ready                                                                   |
| `ready`                   | nftables loaded + self-test passed; eligible for scheduling                                                |
| `isolation_unavailable`   | nftables ruleset failed; node refuses container scheduling; other nodes skip it                            |
| `drain_timeout`           | `jaco node remove` aborted after the 5-minute per-replica drain timeout                                    |

`jaco cluster status` and `jaco node list` render the trimmed names
(no `NODE_STATUS_` prefix).

## Rollout state

`RolloutState` (per service undergoing a rolling update):

| state           | meaning                                                            |
|-----------------|--------------------------------------------------------------------|
| `in_progress`   | the scheduler is advancing one replica at a time                   |
| `completed`     | all steps applied; the new revision is steady-state                 |
| `aborted`       | a step timed out; previous revision continues to serve              |

## Audit event types

`AuditEventType` lives at
[`proto/jaco/v1/entities.proto`](../../proto/jaco/v1/entities.proto):

`apply`, `delete`, `rollback`, `node_join`, `node_remove`,
`token_issue`, `token_revoke`, `certificate_issued`,
`certificate_renewed`, `certificate_failed`,
`isolation_ruleset_reconciled`, `isolation_unavailable`,
`backup_taken`, `restore_completed`, `upgrade_succeeded`,
`upgrade_failed`, `rollout_invariant_hold`.

`jaco audit --type <name,…>` filters on the short forms. See
[`jaco audit`](../cli/audit.md).

## See also

- [`jaco audit`](../cli/audit.md), [`jaco status`](../cli/status.md)
- [Troubleshooting](../operations/troubleshooting.md)
- [Auth and tokens](auth-and-tokens.md)
