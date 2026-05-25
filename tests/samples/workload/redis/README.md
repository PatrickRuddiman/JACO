# redis tier — primary + read replicas

The workload's stateful tier is Redis in a **primary + N replicas** topology
(async replication, no sharding):

- **`redis-primary`** — 1 instance, accepts writes. The api writes notes here.
- **`redis-replica`** — N instances (default 2), `replicaof redis-primary`,
  read-only. The api reads the notes list here; the orchestrator load-balances
  the `redis-replica` service across all replica instances.

This maps cleanly to every orchestrator's `replicas: N` primitive, so the same
topology is expressed identically on JACO, Kubernetes, k3s, and Docker Swarm —
which is what keeps the cross-stack comparison fair.

## Config delivery

Bind-mounting a `.conf` file is **not portable across a multi-node cluster**
(the file would have to exist on every node), so the multi-node manifests pass
the same settings as **command flags** instead:

```
redis-primary:  redis-server --save "" --appendonly no --protected-mode no
redis-replica:  redis-server --replicaof redis-primary 6379 --replica-read-only yes \
                             --save "" --appendonly no --protected-mode no
```

The `primary.conf` / `replica.conf` files here are the canonical reference and
are mounted by the single-host `docker compose` dry-run (see
[`../../jaco`](../../jaco)); the flags above are their exact equivalent.

## Notes on fairness

- Persistence is **off** (`save ""`, `appendonly no`) so no disk-tuning choice
  biases one stack. The dataset is in-memory and reset per run.
- No auth (`protected-mode no`): the tier is only reachable on the closed
  in-cluster network. Do not expose it.
- The api exposes observed replication lag (`bench_replica_lag_seconds`) via a
  heartbeat key, so the rubric can compare how each stack's networking affects
  primary→replica propagation.
