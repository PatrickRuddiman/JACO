# Manifest examples

Progressive examples, from a one-service deployment to a multi-network
deployment with public ingress. Every example is a `jaco.yaml` plus a
`docker-compose.yml` pair; copy them into the same directory and run
`jaco apply <jaco.yaml>` — the compose file auto-resolves.

## 0. Routes-only: compose supplies the rest

The slimmest legal `jaco.yaml` (issue #99). No `services:` block —
every compose service inherits a single-replica `spread` placement,
and `deploy.replicas` in the compose file (if set) supplies the
default count. The overlay only declares the public route, because
that's the only thing compose itself can't express.

`jaco.yaml`:

```yaml
deployment: slim
routes:
  - domain: slim.example.com
    service: web
    port: 80
    tls: auto
```

`docker-compose.yml`:

```yaml
services:
  web:
    image: nginx:1.27
    deploy:
      replicas: 3
```

The merged ServiceSpec lands `web` at three replicas, placement
`spread`, networks `[_default]`. Add a `services:` entry to override
any of those (see Example 5 for a multi-service overlay).

## 1. One service, no ingress

The shortest legal pair. Three replicas of nginx, spread across the
cluster, no routes.

`jaco.yaml`:

```yaml
deployment: hello
services:
  - name: web
    replicas: 3
```

`docker-compose.yml`:

```yaml
services:
  web:
    image: nginx:1.27
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1/"]
      interval: 5s
      timeout: 3s
      retries: 5
```

Replicas are reachable east-west by their service name (`web`) inside
the deployment's default network. Not reachable from outside the
cluster — that needs a `routes` entry.

## 2. One service, public route with auto TLS

The canonical sample from `cmd/jaco/testdata/sample.jaco.yaml`. Adds a
public route so the embedded Caddy on every node serves
`web.example.com` from a healthy replica of `web`.

`jaco.yaml`:

```yaml
deployment: sample
services:
  - name: web
    replicas: 2
routes:
  - domain: web.example.com
    service: web
    port: 80
    tls: auto
```

`docker-compose.yml`: same as example 1.

For `tls: auto` to succeed, DNS for `web.example.com` must resolve to
at least one cluster node's public IP. Until then, the cert state in
`jaco status` stays `pending` with retry backoff and plaintext HTTP
continues to serve.

## 3. Host pinning

A database-style workload that must run only on specific hosts (perhaps
because they have the right disk).

```yaml
deployment: data
services:
  - name: db
    replicas: 1
    placement: hosts
    hosts: [storage-1]
```

```yaml
services:
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
volumes:
  pgdata:
```

`placement: hosts` with a single host means a single home. If
`storage-1` is unreachable, `jaco status data` reports `pending:
cannot satisfy host placement: storage-1 unreachable` and no replica is
scheduled elsewhere.

## 4. Two networks inside one deployment

Same pattern as `testdata/isolation/dep-front.jaco.yaml` (used by the
isolation rig). Two services attached to two disjoint networks; they
cannot reach each other.

`jaco.yaml`:

```yaml
deployment: split
services:
  - name: svc-a
    replicas: 2
    networks: [net-a]
  - name: svc-b
    replicas: 2
    networks: [net-b]
```

`docker-compose.yml`:

```yaml
services:
  svc-a:
    image: busybox:1.36
    command: ["sh", "-c", "nc -lk -p 9999 -e /bin/sh"]
    networks: [net-a]
  svc-b:
    image: busybox:1.36
    command: ["sh", "-c", "nc -lk -p 9998 -e /bin/sh"]
    networks: [net-b]
networks:
  net-a: {}
  net-b: {}
```

`svc-a → svc-b` resolution returns NXDOMAIN; even by-IP attempts are
dropped at FORWARD by the per-(deployment, network) nftables rules.
See [Isolation](../concepts/isolation.md).

## 5. Multi-tier with shared and segregated networks

A web tier on a frontend network, a db on a backend network, a
gateway service that bridges both.

```yaml
deployment: app
services:
  - name: web
    replicas: 3
    networks: [frontend]
  - name: gateway
    replicas: 2
    networks: [frontend, backend]
  - name: db
    replicas: 1
    placement: hosts
    hosts: [storage-1]
    networks: [backend]
routes:
  - domain: app.example.com
    service: web
    port: 80
    tls: auto
  - domain: app.example.com
    service: gateway
    port: 8080
    tls: auto
    path: /api/
```

```yaml
services:
  web:
    image: nginx:1.27
    networks: [frontend]
  gateway:
    image: ghcr.io/example/gateway:1.0
    networks: [frontend, backend]
    environment:
      DB_HOST: db
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data
    networks: [backend]
volumes:
  pgdata:
networks:
  frontend: {}
  backend: {}
```

- `web` reaches `gateway` over `frontend`; `gateway` reaches `db` over
  `backend`. `web` cannot reach `db` — they share no network.
- Two routes for `app.example.com` co-exist because their paths differ
  (`/api/` and the implicit `""` catch-all). The longer prefix wins.

## 6. Raw-TCP ingress alongside HTTP

A redis service exposed publicly on TCP 6379 from every node; plus a
web tier on HTTPS.

```yaml
deployment: cache
services:
  - name: redis
    replicas: 1
    placement: hosts
    hosts: [storage-1]
  - name: web
    replicas: 2
routes:
  - domain: cache.example.com
    service: web
    port: 80
    tls: auto
```

```yaml
services:
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
  web:
    image: ghcr.io/example/cache-ui:1.0
networks: {}
```

The redis `ports: ["6379:6379"]` declares a cluster-wide TCP listener:
every node accepts TCP on `:6379` and forwards to the (single) redis
replica wherever it runs. Failover to a surviving replica is the same
mechanism as HTTP — within the route-removal window after the
unhealthy replica is observed.

Publishing 80 or 443 is rejected — those belong to the Caddy ingress.

## See also

- [`jaco.yaml` schema](jaco-yaml.md)
- [Supported compose fields](compose.md)
- [`jaco apply`](../cli/apply.md)
- [Networking](../concepts/networking.md)
- [Ingress](../concepts/ingress.md)
