---
sources:
  - cmd/jaco/token.go
  - internal/controlplane/grpc/token.go
  - internal/controlplane/admission/
---

# `jaco token`

Operator-token management. All three subcommands currently require
`--server` plus an existing operator token to authenticate the
operation.

## `jaco token issue`

### Synopsis

```
jaco token issue --server <host:port> --name <identity> [--token <op>] [--ca-cert <path>]
```

### Flags

| flag                  | default                       | meaning                                  |
|-----------------------|-------------------------------|------------------------------------------|
| `--server <addr>`     | — (required)                  | leader gRPC                              |
| `--name <s>`          | — (required)                  | identity for the new token               |
| `--token <op>`        | `JACO_TOKEN`                  | calling operator's bearer token          |
| `--ca-cert <path>`    | `/var/lib/jaco/node/ca.crt`   | cluster CA PEM                           |

### Auth

Operator token, required.

### Behavior

Mints a new opaque bearer token bound to `--name` (e.g. `alice`,
`ci-deploy`). The plaintext is printed once on stdout; only the SHA-256
hash is stored in raft as a `Token{identity, hashed_secret, issued_at}`
entity. Subsequent state-changing RPCs presented with this token are
attributed to `<name>` in the audit log.

### Exit codes

- `0` — token issued.
- `1` — auth, transport, or duplicate-identity error.

### Examples

```sh
jaco token issue --server $LEADER --name ci-deploy
# Token for ci-deploy (save this; not recoverable): 1b2c...
```

## `jaco token revoke`

### Synopsis

```
jaco token revoke <identity> --server <host:port> [--token <op>] [--ca-cert <path>]
```

### Flags

Same as `issue`, minus `--name`.

### Auth

Operator token, required.

### Behavior

Marks the token as revoked (`revoked_at = now`). Revocation is a raft
write applied on every node; subsequent RPCs presented with the
revoked token return `Error{code: token_revoked}` cluster-wide within
one apply (well under 5 s, satisfying the spec's
5-second-revocation-propagation bar).

### Exit codes

- `0` — revoked.
- `1` — unknown identity, auth, or transport error.

### Examples

```sh
jaco token revoke --server $LEADER ci-deploy
# Revoked token for ci-deploy
```

## `jaco token list`

### Synopsis

```
jaco token list --server <host:port> [--token <op>] [--ca-cert <path>]
```

### Flags

Same as `revoke`.

### Auth

Operator token, required.

### Behavior

Prints one row per known token: identity, issued-at timestamp, and
revoked-at timestamp (or `-` if active). Hashes are never disclosed,
and the original plaintext token is unrecoverable.

### Exit codes

- `0` — list printed.
- `1` — auth or transport error.

### Examples

```sh
jaco token list --server $LEADER
# IDENTITY                       ISSUED               REVOKED
# bootstrap                      2026-05-01T12:00:00Z -
# ci-deploy                      2026-05-02T09:14:00Z 2026-05-24T08:00:00Z
# alice                          2026-05-10T15:32:00Z -
```

## See also

- [Auth and tokens](../concepts/auth-and-tokens.md)
- [`jaco audit`](audit.md)
- [`jaco node`](node.md) — `issue-join-token` for cluster-membership tokens
