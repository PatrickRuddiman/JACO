# `environment:` interpolation sample

Smallest end-to-end shape for the top-level `environment:` field in
`jaco.yaml`. The CLI loads `.env`, interpolates every `${VAR}` in the
compose document against those values, and ships resolved bytes to the
daemon — the daemon never sees `.env`.

Files:

- `jaco.yaml` — points at `.env` via `environment: .env`, declares one route.
- `compose.yml` — consumes `${REGISTRY}`, `${DB_URL}`, and `${AWS_REGION:-us-east-1}`.
- `.env` — supplies `REGISTRY` and `DB_URL`; the `AWS_REGION` default fires.

Apply:

```sh
jaco apply tests/samples/jaco/env-interpolation/jaco.yaml
```

What lands on the daemon (after CLI-side interpolation):

```yaml
services:
  api:
    image: ghcr.io/myorg/api:1
    environment:
      DB_URL: postgres://db.internal/env-demo
      REGION: us-east-1
```

See [`docs/manifests/jaco-yaml.md#environment`](../../../docs/manifests/jaco-yaml.md#environment)
for the full semantics (path resolution, precedence with service-level
`env_file:`, no process-environment passthrough).
