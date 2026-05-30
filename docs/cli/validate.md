---
sources:
  - cmd/jaco/validate.go
  - internal/controlplane/grpc/jaco_spec.go
  - internal/runtime/compose/validate.go
---

# `jaco validate`

Validate `jaco.yaml` and/or compose files locally. Runs the same
admission validators the daemon uses on apply, so a `validate` pass
that succeeds locally is sufficient signal that the apply will not be
rejected on schema grounds.

## Synopsis

```
jaco validate [--jaco <path>] [--compose <path>]
```

## Flags

| flag              | default | meaning                          |
|-------------------|---------|----------------------------------|
| `--jaco <path>`   | —       | path to a `jaco.yaml` manifest   |
| `--compose <path>`| —       | path to a compose YAML file      |

At least one flag is required. When both are provided, the validator
additionally cross-checks that every `services[*].name` in the jaco
manifest matches a key in the compose file.

## Auth

None — fully local; no cluster contact.

## Behavior

- The jaco manifest is parsed by `internal/controlplane/grpc`'s
  validator. Unknown top-level keys, unknown service-level keys, bad
  placement modes (the closed set is `spread | pack | hosts |
  global`), or missing required fields fail with `validation_failed`
  and a typed message.
- The compose file is parsed by `internal/runtime/compose`'s
  validator. Service-level fields outside the supported allowlist
  fail with `validation_failed` listing the offending service and
  field. Networks referenced under a service but absent from the
  top-level `networks:` block fail with `unknown_network`. Services
  that publish reserved host ports (80 or 443) fail with
  `reserved_port`. See
  [manifests/compose.md](../manifests/compose.md) for the closed
  field set.
- Cross-check failures (jaco service name not in compose) fail with
  `validation_failed: jaco service "X" is not defined in the compose
  file`.

Errors render as `Error: <code>: <message>` on stderr.

## Exit codes

- `0` — every requested file is valid.
- `1` — validation failure or filesystem error.

## Examples

```sh
jaco validate --jaco ./hello/jaco.yaml --compose ./hello/docker-compose.yml

# Compose-only sanity check:
jaco validate --compose ./hello/docker-compose.yml

# CI usage: lint before applying, exit non-zero on bad pairs.
jaco validate --jaco $f --compose $(dirname $f)/docker-compose.yml || exit 1
```

## See also

- [`jaco apply`](apply.md)
- [Manifest schema](../manifests/jaco-yaml.md)
- [Supported compose fields](../manifests/compose.md)
