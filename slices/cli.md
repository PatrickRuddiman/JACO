Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — cli

## §1 Summary

The `jaco` binary's subcommand surface for operators and developers. Same binary as the daemon (`jaco serve` runs the daemon); when invoked as any other subcommand the process is a short-lived client. Owns context/token storage, endpoint rotation, gRPC client setup, output formatting, and streaming UX (`logs`, `audit tail`, `status -w`).

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Subcommand framework.** Options: spf13/cobra+viper, urfave/cli/v2, alecthomas/kong. **Chosen:** cobra + viper. Rationale: the Go ecosystem default; shell completions, env binding, and config file integration are built in; pattern is familiar to operators coming from kubectl/docker/gh.
2. **Default output format.** Options: table by default with `--output json|yaml`, JSON-by-default, auto-detect TTY. **Chosen:** table by default, `-o json|yaml` flag. Rationale: matches kubectl/docker/gh; predictable across TTY and pipe contexts.
3. **Token + endpoint storage.** Options: per-cluster context file with env override, env-only, single-cluster token+config file. **Chosen:** `~/.config/jaco/clusters.yaml` holding named cluster contexts; `JACO_TOKEN` / `JACO_SERVER` / `JACO_CA_CERT` env vars override. Rationale: kubectl pattern, scales to multiple clusters, CI ergonomic.
4. **Endpoint rotation strategy.** Options: static list per context with fallthrough, single address, DNS SRV. **Chosen:** static address list per context; CLI tries each address in order on connection error or `no_leader`. Rationale: matches the "any node accepts commands" spec promise; no external DNS dependency.

## §4 Contracts & shapes

Module layout under `cmd/jaco/`:

- `cmd/jaco/main.go` — entry point; constructs root cobra command, dispatches to subcommands.
- `cmd/jaco/serve.go` — `jaco serve` boots the daemon (control-plane + every other vertical's worker). All other files in this slice are client-mode commands.
- `cmd/jaco/bootstrap.go`, `node.go` (join, remove, list), `apply.go`, `rollback.go`, `delete.go`, `status.go`, `logs.go`, `audit.go`, `token.go` (issue, revoke, list), `backup.go`, `restore.go`, `self_upgrade.go`.
- `internal/cliclient/` — gRPC client builder: loads context, opens TLS connection with cluster CA, attaches bearer token, rotates addresses on retryable errors.
- `internal/cliclient/context.go` — `~/.config/jaco/clusters.yaml` loader/writer with file-mode-must-be-0600 check.
- `internal/cliclient/output.go` — render helpers: `RenderTable`, `RenderJSON`, `RenderYAML`; one entry point per command type.

Clusters config file shape (`~/.config/jaco/clusters.yaml`):

- `current_context: prod`
- `contexts: [{name: prod, server_addrs: [n1.example:7000, n2.example:7000, n3.example:7000], ca_cert_path: /path/to/cluster-ca.crt, token: <opaque>}]`
- File must be `0600`; CLI refuses to read it otherwise.

Env var overrides (each fully replaces the matching context field):

- `JACO_CONTEXT` — context name to use instead of `current_context`.
- `JACO_SERVER` — single address or comma-separated list, replaces `server_addrs`.
- `JACO_TOKEN` — replaces context token.
- `JACO_CA_CERT` — path to CA cert PEM file, replaces `ca_cert_path`.

Global flags (registered on root command via cobra):

- `--context <name>`
- `--output, -o {table|json|yaml}` (default `table`)
- `--server <addr>` (single-shot override; bypasses context)
- `--quiet, -q` (suppress non-essential output)
- `--verbose, -v` (debug-level logs to stderr; W3C traceparent injected)

Subcommand surface (full set):

- `jaco serve [--data-dir=/var/lib/jaco] [--bind=0.0.0.0:7000] [--otlp-endpoint=]` — runs the daemon.
- `jaco bootstrap --name <hostname>` — emits the initial operator token on stdout once.
- `jaco node join --address <host:port> --join-token <secret> --name <hostname>`
- `jaco node remove <hostname> [--force]`
- `jaco node list`
- `jaco apply <jaco.yaml> [--dry-run]` — prints diff and exits when `--dry-run`; otherwise applies and waits for the apply RPC to return success or a typed error.
- `jaco rollback <deployment>`
- `jaco delete <deployment>`
- `jaco status [deployment[/service]] [-w]` — `-w` opens a watch stream and re-renders on change.
- `jaco logs <deployment>/<service> [-f] [--since 1h]` — `-f` streams.
- `jaco audit [--since 1h] [--type apply,delete] [-f]`
- `jaco token issue --name <identity>`
- `jaco token revoke <identity>`
- `jaco token list`
- `jaco backup --output cluster.tar.gz`
- `jaco restore --input cluster.tar.gz --name <hostname>`
- `jaco self-upgrade --url <https://…/jaco-vX.Y.Z>`

Output rendering rules:

- Tables: column set per command, no wrapping; truncation indicated by `…`. ANSI color only on TTY.
- JSON: pretty-printed, stable key order (alphabetical), one object per top-level call. Streaming RPCs emit one JSON object per line (NDJSON).
- YAML: same data as JSON, gopkg.in/yaml.v3.

Error rendering:

- Typed `Error{code, message, details}` from the gRPC envelope renders as `Error: <code> — <message>` on stderr, with `details` rendered as key=value pairs. Exit code 1 on any non-empty error.
- Transport-layer failures (connection refused, TLS verify failed) render as `Connection error: <addr>: <reason>` and the CLI tries the next address.

## §5 Sequence

Client startup (every non-`serve` subcommand):

1. cobra parses args; root command's PersistentPreRun loads `~/.config/jaco/clusters.yaml`, applies env overrides, resolves the effective context.
2. `internal/cliclient` builds a gRPC client: TLS dialer with cluster CA cert, bearer token unary interceptor (`authorization: Bearer <token>`), default deadline 30 s for unary RPCs / no deadline for streams.
3. Subcommand handler runs, calls the appropriate stub on the client.
4. On success: render via `internal/cliclient/output` selected by `-o`. On error: render via the error renderer; exit non-zero.

Endpoint rotation:

1. Client dials `server_addrs[0]`. On TCP/TLS failure or `Error{code: no_leader}`, the unary interceptor cycles to `server_addrs[1]`, retries.
2. If all addresses exhausted within the deadline, returns `Connection error: all endpoints unreachable: [list]`.
3. Streaming RPCs do not rotate mid-stream; on stream break, the user re-runs the command.

`jaco apply --dry-run`:

1. CLI reads `jaco.yaml` + referenced compose file from local disk.
2. Calls `Deploy.Apply(yaml_bytes, dry_run=true)`.
3. Server validates against entity schema and current state, returns a `Diff` proto: `{adds, updates, removes}` for replicas/routes/certs.
4. CLI renders the diff as a table grouped by entity type; exit 0 if diff is non-empty and valid; exit 0 with `No changes` if empty.

`jaco logs <deployment>/<service> -f`:

1. CLI opens `Deploy.Logs(deployment, service, follow=true, since=…)`.
2. Server-side fanout (entry node) opens peer streams; merges; pushes back to CLI.
3. CLI renders each line: `[<replica-id>@<host>] <line>`; flushes immediately. Ctrl-C closes the stream cleanly.

`jaco status -w`:

1. CLI calls `Deploy.Status` once for the initial snapshot.
2. Then opens a `Watch.Subscribe(entity_type=deployments)` stream filtered to the requested deployment.
3. On each `Event{Updated|Added|Removed}`, CLI clears and re-renders the table. On `Event{Resync}`, CLI re-fetches via `Deploy.Status`.

## §6 Out of scope

- Daemon-side handling of any subcommand (lives in control-plane, scheduler, runtime, ingress, discovery, packaging slices).
- Shell completion scripts beyond what cobra generates; no custom completers in v1.
- Plugin commands (`jaco-<plugin>` PATH discovery, kubectl-style).
- Color theming / config beyond TTY detection.
- A web UI (spec §3 Out).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
