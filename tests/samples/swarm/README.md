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
| north-south ingress | Caddy route → web (TLS) | routing mesh publishes `web:80` on every node |

## Ingress / TLS

Swarm has no built-in TLS terminator, so `web` is published on **:80** via the
routing mesh (reachable on any node behind the testbed LB). Bench it over HTTP —
keep this constant across stacks for a fair comparison (see
[RUBRIC.md](../bench/RUBRIC.md)):

```sh
cd tests/samples/bench
export BENCH_PUBLIC_IPS="..." BENCH_PRIVATE_IPS="..."
export BENCH_TARGET="http://<lb-ip>" BENCH_HOST_HEADER="jaco.sh"
./run.sh swarm
./collect.sh
```

## Verify

```sh
ssh azureuser@<n1-pub> 'sudo docker stack services bench'   # all replicas converged
curl -s -H 'Host: jaco.sh' http://<lb-ip>/api/notes          # JSON list (reads a redis replica)
curl -s -H 'Host: jaco.sh' http://<lb-ip>/api/metrics        # Prometheus metrics (incl. pg replica lag)
```

> The `node.hostname` placement constraints assume the default `jaco-1/2/3`
> names — adjust in `stack.yml` if you deploy under a different prefix.
