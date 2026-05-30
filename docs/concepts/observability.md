---
sources:
  - internal/logging/
  - internal/controlplane/grpc/audit.go
  - cmd/jacod/main.go
  - proto/jaco/v1/entities.proto
---

# Observability

JACO emits three streams of telemetry: **OpenTelemetry traces +
metrics**, **structured logs**, and the **cluster audit log** in raft
state. The OTel SDK is opt-in via env; the logs and audit log are
always on.

## OpenTelemetry

### Exporter

OTLP endpoint comes from `JACO_OTLP_ENDPOINT`. **When unset, the
OpenTelemetry SDK is disabled entirely** — no metrics collection
overhead, no spans created, no exporter goroutine. Set the env on
`jacod` (e.g. via the systemd unit override) to enable.

### Traces

All RPCs (CLI ↔ node and node ↔ node) propagate W3C `traceparent`.
End-to-end span chain for a successful apply:

```
cli.apply
  → raft.commit
    → raft.apply
      → scheduler.reconcile.<service>
        → runtime.<docker_op>
```

Same chain instruments rollback and delete. Slow applies attribute to a
specific stage — `raft.commit` (write replication), `scheduler.reconcile`
(placement + diff), `runtime.<docker_op>` (image pull, container
create).

### Span attributes (required)

Every span carries:

- `jaco.cluster_id`
- `jaco.node`
- `jaco.deployment`
- `jaco.service`
- `jaco.replica_id`
- `jaco.identity` — the resolved operator identity (or `local` /
  `system`)

### Metric names

Predefined cluster-health and performance metrics:

- `jaco_raft_commit_latency_seconds`
- `jaco_apply_duration_seconds`
- `jaco_scheduler_reconcile_lag_seconds`
- `jaco_replica_state{service, state}`
- `jaco_ingress_requests_total`
- `jaco_ingress_duration_seconds`
- `jaco_cert_renewals_total`
- `jaco_runtime_container_starts_total`
- `jaco_token_revocation_propagation_seconds`

## Logs

Structured via stdlib `log/slog`. The convention lives in
[`internal/logging/`](../../internal/logging/logging.go).

### Daemon (`jacod`)

- **Under systemd with journal socket reachable**: native journald
  protocol. `PRIORITY` (debug=7, info=6, warn=4, error=3),
  `SYSLOG_IDENTIFIER=jacod`, and every slog attribute become real
  queryable journal fields:

  ```sh
  journalctl -u jaco -p err
  journalctl SUBSYSTEM=raft
  journalctl SUBSYSTEM=scheduler -f
  ```

- **Under systemd without a journal socket**: JSON to stderr (one
  object per line). Systemd still captures.
- **Outside systemd**: human-readable text to stderr.

systemd detection is via the `INVOCATION_ID` env var.

### CLI (`jaco`)

Human-readable text to stderr, default level `warn` (operator output
stays uncluttered). Override:

```
--log-level debug   # or info / warn / error
-v / --verbose      # equivalent to --log-level debug
JACO_LOG=info       # process-level
```

Precedence: `--log-level` > `--verbose` > `JACO_LOG` > `warn`.

### Subsystem keys

Every log line carries a `subsystem` attribute set by the package
that emitted it:

`raft`, `scheduler`, `runtime`, `discovery`, `ingress`,
`controlplane`, `admission`, `bootstrap`, `wgmesh`, `firewall`,
`dns`, `health`, …

Filtering on subsystem (`SUBSYSTEM=firewall` under journald,
`jq 'select(.subsystem=="firewall")'` on the JSON path) is the
fastest way to triage.

### Sensitive data

Bearer tokens, private keys, and audit-event payloads are NEVER passed
as log attributes. The logging package does not (and cannot) scrub
them; callers are responsible. The CLI specifically logs only the
selected transport + target on operator dials (never the token).

## Audit log

A separate, append-only stream stored in raft (`AuditEvent{ts, type,
identity, payload}`). Distinct from process logs because the events
are part of the cluster's durable state, not artifacts of a single
daemon instance.

Closed event types live in
[`proto/jaco/v1/entities.proto`](../../proto/jaco/v1/entities.proto)
and are listed under
[Status and errors](status-and-errors.md). Query via:

```sh
jaco audit --server $LEADER --since 1h --type apply,delete,token_revoke
```

System-driven events (`isolation_ruleset_reconciled`,
`certificate_renewed`, `backup_taken`) use `identity: system`.
Socket-trust RPCs use `identity: local`. Token-authenticated RPCs use
the bound identity.

## Operational recommendations

- Set `JACO_OTLP_ENDPOINT` on every `jacod` in production and feed
  metrics into your existing observability stack (Prometheus via OTel
  collector, Tempo for traces, Loki for logs).
- Use `jaco audit -f` to tail high-signal events (token revocation,
  certificate failures, isolation drift) during incidents.
- For one-off investigation, `jaco status -w` plus `jaco logs <dep>/<svc>
  -f` is faster than spinning up a dashboard.

## See also

- [`jaco audit`](../cli/audit.md)
- [Status and errors](status-and-errors.md)
- [Troubleshooting](../operations/troubleshooting.md)
- [Configuration](../configuration.md) — `log_level`
