# Docker Swarm — follow-up pass

**Status: stubbed.** The shared [workload](../workload) and the
[bench harness](../bench) are in place; this stack's bootstrap + manifests land
in the follow-up.

## Planned bootstrap (from the minimal Debian baseline)

1. Install Docker on all three nodes (`get.docker.com`).
2. node-1: `docker swarm init --advertise-addr <node1-priv>` → capture the
   worker join-token.
3. node-2/3: `docker swarm join --token <token> <node1-priv>:2377`.
4. Stand up the in-cluster registry on node-1 and push the `web`/`api` images
   (same as the JACO flow — Swarm pulls from a registry too).
5. `docker stack deploy -c stack.yml bench`.

## Planned `stack.yml`

A near-copy of [`../jaco/docker-compose.yml`](../jaco/docker-compose.yml): the
same services, `deploy.replicas` (web 2 / api 2 / redis-primary 1 /
redis-replica 2), and `deploy.resources.limits`. Swarm's routing mesh publishes
`web` on host 80/443, so it sits behind the testbed LB directly. The `web`
nginx `resolver 127.0.0.11` works as-is on Swarm (docker embedded DNS), so the
workload images are byte-identical to the JACO run here.

The bench adapter (`../bench/adapters/swarm.sh`) will wrap this so
`../bench/run.sh swarm` works end-to-end. Swarm is the closest architectural
sibling to JACO (docker-native, no separate control-plane install), which makes
it the most direct comparison.
