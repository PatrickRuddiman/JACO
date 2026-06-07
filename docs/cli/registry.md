---
sources:
  - cmd/jaco/registry.go
  - internal/controlplane/grpc/registry_credential.go
  - internal/controlplane/state/registry_credentials.go
  - internal/controlplane/watch/registry.go
---

# `jaco registry`

Manage container-registry credentials that are replicated across the
raft and consumed by every node's image-pull path. With a credential in
place for a host, the cluster authenticates pulls against private
registries — Docker Hub private repos, GHCR, ECR, self-hosted — without
needing per-node `.docker/config.json` setup.

Issue: [#101](https://github.com/PatrickRuddiman/jaco/issues/101).
Background: [Auth and tokens — Registry
credentials](../concepts/auth-and-tokens.md#registry-credentials).

## Canonical credential keys

A credential is keyed by its canonical `host[:port][/namespace]`. Both
`jaco registry login` and the reconciler's per-pull lookup normalize the
input the same way, so a credential added under one alias is found by
every pull whose image points at any other alias for the same key.

| input                                | canonical key             |
|--------------------------------------|---------------------------|
| `docker.io`, `index.docker.io`       | `docker.io`               |
| `ghcr.io`                            | `ghcr.io`                 |
| `ghcr.io/Owner/`, `GHCR.IO/owner`    | `ghcr.io/owner`           |
| `registry.example.com:5000`          | `registry.example.com:5000` |
| `https://ghcr.io/owner?ref=main`     | `ghcr.io/owner`           |

Hosts are lower-cased; non-default ports are preserved verbatim. Any
`scheme://`, query, or fragment is stripped, and path segments are
lower-cased and trim-of-trailing-slash.

### Per-namespace credentials

The optional `/namespace` suffix lets a single host carry distinct
credentials per registry namespace — e.g. `ghcr.io/personal` for a
developer account and `ghcr.io/company` for a CI service principal,
stored alongside a bare `ghcr.io` fallback if you want one.

At pull time the resolver picks the **longest-prefix match** against
the image's `host[:port]/<repository-path>`:

- `ghcr.io/personal/repo:tag` → uses `ghcr.io/personal` if present;
  otherwise falls back to `ghcr.io` if that's stored; otherwise pulls
  anonymously.
- `ghcr.io/company/app:tag` → uses `ghcr.io/company`.
- `ghcr.io/orphan/x:tag` → uses bare `ghcr.io` only (no namespace
  match).

More-specific keys always win over the bare-host key, so it is safe to
keep a fallback `ghcr.io` credential alongside namespace-scoped ones.

## `jaco registry login`

### Synopsis

```
jaco registry login <registry> --username <name> [--password-stdin]
                    [--server <host:port>] [--token <op>] [--ca-cert <path>] [--socket <path>]
```

### Flags

| flag                  | default                       | meaning                                  |
|-----------------------|-------------------------------|------------------------------------------|
| `<registry>`          | — (positional, required)      | registry host (canonicalized server-side)|
| `--username <name>`   | — (required)                  | registry username                        |
| `--password-stdin`    | `false`                       | read password from stdin instead of prompting |
| `--server <addr>`     | (unset → unix socket)         | leader gRPC; off-node only               |
| `--token <op>`        | `JACO_TOKEN`                  | operator bearer; required with `--server`|
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                           |
| `--socket <path>`     | `$JACO_SOCKET` or default     | local jacod unix socket                  |

The password is **never** taken on the command line — passing it that
way would leak it via `/proc/<pid>/cmdline` and shell history. The CLI
prompts on a TTY with echo suppressed, or reads stdin with
`--password-stdin`.

### Auth

Local (on-node) → unix socket, no token. Remote → operator token over
TLS.

### Behavior

Upserts the credential under the canonical key. A second `login` for
the same host replaces the secret (this is the rotation path; there is
no separate `rotate` verb). The FSM emits an
`AUDIT_EVENT_TYPE_REGISTRY_CREDENTIAL_UPSERT` audit event carrying
`registry` and `username` — the secret is never written to the audit
trail.

### Exit codes

- `0` — credential stored.
- `1` — empty password, auth/transport error, or leader unavailable.

### Examples

```sh
# Interactive prompt (TTY only)
jaco registry login ghcr.io --username ci-bot
# Password: ********

# Piped from a CI secret store
echo "$GHCR_PAT" | jaco registry login ghcr.io --username ci-bot --password-stdin

# From a remote operator workstation
jaco registry login docker.io --username alice --password-stdin \
  --server $LEADER --token $JACO_TOKEN < ~/secrets/dockerhub

# Per-namespace credentials on the same host — these coexist; pulls of
# ghcr.io/personal/* and ghcr.io/company/* each see their own creds, and
# anything under ghcr.io/<other-namespace>/* picks up the bare-host one.
echo "$PERSONAL_PAT" | jaco registry login ghcr.io/personal --username alice    --password-stdin
echo "$COMPANY_PAT"  | jaco registry login ghcr.io/company  --username ci-bot   --password-stdin
echo "$FALLBACK_PAT" | jaco registry login ghcr.io          --username fallback --password-stdin
```

## `jaco registry logout`

### Synopsis

```
jaco registry logout <registry> [--server <host:port>] [--token <op>] [--ca-cert <path>] [--socket <path>]
```

### Flags

Same transport flags as `login`, minus `--username` and
`--password-stdin`.

### Auth

Local → unix socket. Remote → operator token.

### Behavior

Removes the credential under the canonical key. Idempotent: removing
an unknown host returns success without error. Emits
`AUDIT_EVENT_TYPE_REGISTRY_CREDENTIAL_REMOVE`.

### Exit codes

- `0` — removed (or not present).
- `1` — auth/transport error, or leader unavailable.

### Examples

```sh
jaco registry logout ghcr.io
# Removed credential for ghcr.io
```

## `jaco registry list`

### Synopsis

```
jaco registry list [--server <host:port>] [--token <op>] [--ca-cert <path>] [--socket <path>]
```

### Flags

Same transport flags as `logout`.

### Auth

Local → unix socket. Remote → operator token.

### Behavior

Prints one row per known credential: registry, username, last-updated
timestamp. **The secret is never printed.** The list path returns a
`RegistryCredentialSummary` message that has no secret field — the
wire type itself enforces redaction. Operators rotate by re-running
`login` with the new secret; there is no read-back of the existing
secret.

### Exit codes

- `0` — list printed (possibly empty).
- `1` — auth or transport error.

### Examples

```sh
jaco registry list
# REGISTRY                                 USERNAME                       UPDATED
# docker.io                                alice                          2026-05-20T11:00:00Z
# ghcr.io                                  ci-bot                         2026-05-24T08:14:00Z
# registry.example.com:5000                deploy                         2026-05-18T09:00:00Z
```

## See also

- [Auth and tokens — Registry credentials](../concepts/auth-and-tokens.md#registry-credentials)
- [`jaco audit`](audit.md) — surfaces upsert/remove events
- [`jaco token`](token.md) — operator tokens (the auth this command requires)
