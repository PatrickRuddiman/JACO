# Docker Swarm — bench stack

Deploys the shared [workload](../workload) on Docker Swarm, for the comparative
benchmark ([#51](https://github.com/PatrickRuddiman/JACO/issues/51)). Swarm is
the closest architectural sibling to JACO (docker-native, no separate
control-plane install), so the images and deployment shape are byte-identical to
the [JACO sample](../jaco) — only the orchestration differs.

## Files

```
swarm/
├── stack.yml             # compose-v3 stack: services + deploy (replicas, limits, placement)
├── Caddyfile             # Caddy ingress config: TLS for jaco.sh via ACME staging → web
└── bootstrap/
    ├── bootstrap.sh      # operator-side: install + registry + build/push + swarm init/join + deploy
    └── install-node.sh   # runs on each node: docker + insecure registry
```

## One-shot

From the operator host, with the testbed deployed:

```sh
export BENCH_PUBLIC_IPS="<n1-pub> <n2-pub> <n3-pub>"   # node-1 first
export BENCH_PRIVATE_IPS="<n1-priv> <n2-priv> <n3-priv>"
tests/samples/swarm/bootstrap/bootstrap.sh
```

In order: installs Docker + the insecure registry on every node; stands up
`registry:2` on node-1 and **builds the workload images there** (`bench-web` /
`bench-api` / `bench-postgres` → `<node-1-private-ip>:5000`); `docker swarm init`
on node-1, `docker swarm join` the others as workers; substitutes the registry
into `stack.yml` and `docker stack deploy`s it.

## How it maps to the JACO deployment

| concern | JACO | Swarm |
|---------|------|-------|
| replicas / limits | `jaco.yaml` + compose `deploy.resources` | `stack.yml` `deploy.replicas` + `deploy.resources.limits` |
| service discovery | per-deployment bridge + embedded DNS | overlay network + per-service VIP (same `127.0.0.11` DNS — web image unchanged) |
| pg primary/replica on different nodes | `placement: hosts [jaco-2]/[jaco-3]` | `deploy.placement.constraints: node.hostname == jaco-2 / jaco-3` |
| north-south ingress | Caddy route → web (TLS) | Caddy ingress → web (TLS, ACME staging), `:80`/`:443` via routing mesh |

## Ingress / TLS

Swarm has no built-in TLS terminator, so the stack runs its own **Caddy** service
([`Caddyfile`](Caddyfile)) that terminates TLS for `jaco.sh` and reverse-proxies
to `web` — the same shape as JACO's embedded Caddy, so **both stacks are benched
over HTTPS** (apples-to-apples, per [RUBRIC.md](../bench/RUBRIC.md)). Caddy is a
single replica behind the routing mesh (`:80`/`:443` on every node, all routed to
the one task), which keeps the ACME HTTP-01 challenge on the instance that ordered
the cert — no multi-instance cert coordination (the problem JACO solves with its
raft-backed shared cert store).

ACME is pinned to **Let's Encrypt staging** (`acme_ca` in the Caddyfile): this is a
throwaway bench and prod LE has tight duplicate limits. Staging certs aren't
browser-trusted, so the bench's k6 runs with `insecureSkipTLSVerify` — same as the
JACO run. `jaco.sh` resolves to the testbed LB, so the target is identical to JACO:

```sh
cd tests/samples/bench
export BENCH_PUBLIC_IPS="..." BENCH_PRIVATE_IPS="..."
export BENCH_TARGET="https://jaco.sh"
./run.sh swarm
./collect.sh
```

## Verify

```sh
ssh azureuser@<n1-pub> 'sudo docker stack services bench'   # all replicas converged (incl. caddy 1/1)
curl -sk https://jaco.sh/api/notes      # JSON list (reads a redis replica)
curl -sk https://jaco.sh/api/metrics    # Prometheus metrics (incl. pg replica lag)
```

> The `node.hostname` placement constraints assume the default `jaco-1/2/3`
> names — adjust in `stack.yml` if you deploy under a different prefix.
