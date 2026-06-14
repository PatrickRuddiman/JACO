---
sources:
  - cmd/jaco/getroute.go
  - internal/controlplane/grpc/getroute.go
---

# `jaco get route`

Print the ingress routes Caddy actually serves for a single domain, in the
order it evaluates them, with the live upstream readiness of each route. This
is the operator view behind a domain: it resolves the "why is this domain
returning 503?" and "are these duplicate routes?" questions that the
`jaco status` Routes table cannot fully answer on its own.

## Synopsis

```
jaco get route <domain>
              [--server <host:port> --token <op>]
              [--ca-cert <path>] [--socket <path>]
```

## Flags

| flag               | default                       | meaning                                    |
|--------------------|-------------------------------|--------------------------------------------|
| `--server <addr>`  | —                             | leader gRPC; omit to use the local socket  |
| `--token <op>`     | `JACO_TOKEN`                  | operator bearer token (with `--server`)    |
| `--ca-cert <path>` | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                             |
| `--socket <path>`  | `/var/run/jaco/jaco.sock`     | local jacod unix socket                    |

The single positional argument is the domain to inspect.

## Auth

Operator token (TCP) or unix-socket trust (local).

## Behavior

Returns the realized routes for the domain, ordered the way Caddy evaluates
them: path-scoped routes longest-prefix-first, the catch-all (empty `PATH`)
last. The view is computed from replicated control-plane state, so it is
authoritative and identical on every node. An unknown domain (no routes)
returns a not-found error.

Columns:

- **PATH** — the URL path prefix this route matches; empty means catch-all.
- **SERVICE** — the upstream service the route forwards to.
- **PORT** — the upstream container port.
- **TLS** — `auto` or `off`.
- **STRIP** — `yes` when the matched path prefix is stripped before the
  request reaches the upstream.
- **FALLBACK** — `yes` for the catch-all route (the one that serves any path
  not matched by a more specific prefix).
- **READY** — `ready/total` healthy upstream replicas. A route showing `0/n`
  has no healthy upstream and returns 503 for every matching request — this is
  the silent-503 case to look for.

Routes that share a domain but differ by path are path-scoped, not duplicates;
each row's SERVICE and READY make the intended split explicit.

## Output formats

`-o json` and `-o yaml` emit a structured view instead of the table:

```json
{
  "domain": "web.example.com",
  "routes": [
    {
      "path": "/oauth2",
      "catch_all": false,
      "deployment": "mydeploy",
      "service": "oauth2",
      "port": 4180,
      "tls": "auto",
      "strip_path": false,
      "ready_replicas": 2,
      "total_replicas": 2
    },
    {
      "path": "",
      "catch_all": true,
      "deployment": "mydeploy",
      "service": "website",
      "port": 8080,
      "tls": "auto",
      "strip_path": false,
      "ready_replicas": 0,
      "total_replicas": 1
    }
  ]
}
```

- `tls` is `auto` or `off`.
- `-o yaml` carries the same fields and casing.

Example — fail if any route for a domain has no ready upstream:

```sh
jaco get route web.example.com -o json \
  | jq -e '.routes | all(.ready_replicas > 0)'
```

## Exit codes

- `0` — routes rendered.
- `1` — auth/transport error, or the domain has no routes.

## Examples

Inspect a domain through the leader:

```sh
jaco get route web.example.com --server $LEADER
```

Local-socket lookup on a node:

```sh
sudo jaco get route web.example.com
```

## See also

- [`jaco status`](status.md)
- [`jaco apply`](apply.md)
- [Status and errors](../concepts/status-and-errors.md)
