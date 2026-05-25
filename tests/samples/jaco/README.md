# JACO — deploy the bench workload

Bootstrap a 3-node JACO cluster on the [testbed](../../testbed) and deploy the
[shared workload](../workload).

## Files

```
jaco/
├── jaco.yaml            # deployment, replicas, single ingress route (jaco.sh → web)
├── docker-compose.yml   # service shapes + per-service resource limits
└── bootstrap/
    ├── bootstrap.sh     # operator-side: install, form cluster, build/push, apply
    └── install-node.sh  # runs on each node: docker + jacod + insecure registry
```

## One-shot

From the operator host, with the testbed already deployed:

```sh
# nodes resolved from Azure (RESOURCE_GROUP + VM_NAME_PREFIX), or pass them:
export BENCH_PUBLIC_IPS="<n1-pub> <n2-pub> <n3-pub>"   # node-1 first
export BENCH_PRIVATE_IPS="<n1-priv> <n2-priv> <n3-priv>"
# SSH_KEY auto-resolves to the per-bed key minted at tests/testbed/.ssh/jaco.
tests/samples/jaco/bootstrap/bootstrap.sh
```

What it does, in order:

1. Builds the `jaco_*.deb` (`make package`; needs `nfpm`) — or pass `DEB=`.
2. Installs Docker + jacod on every node and wires the insecure registry.
3. Stands up `registry:2` on node-1 and **builds the workload images there**
   (the operator host can't reach the private registry — only 22/80/443 are
   public), pushing `bench-web` / `bench-api` to `<node-1-private-ip>:5000`.
4. `jaco cluster init` on node-1, issues a join token, `jaco node join` on
   node-2 and node-3 (peer = node-1 private IP `:7000`).
5. `jaco apply` the workload over node-1's local socket.

Mesh traffic (gRPC `:7000`, raft `:7001`, WireGuard `:51820`) stays on the
private VNet; only Caddy ingress (80/443) is public, via the LB.

## Verify

```sh
curl -s https://jaco.sh/                      # UX HTML (or use the LB IP + Host header)
curl -s https://jaco.sh/api/notes             # JSON list (reads a redis replica)
curl -s -XPOST https://jaco.sh/api/notes \
     -H 'content-type: application/json' -d '{"text":"hello"}'   # writes the primary
curl -s https://jaco.sh/api/metrics           # Prometheus metrics from an api replica
```

## Known caveat: multi-node TLS readiness

The ingress LB fronts `:443` on all nodes, but the ACME cert is obtained by the
raft **leader** — followers serve TLS only once that cert propagates to them.
On a freshly-(re)initialized cluster, expect a window where the leader answers
HTTPS while followers still fail the handshake, so LB-fronted requests are
intermittently rejected until propagation completes (observed to take minutes,
and to not complete at all if a cluster's raft state is wiped without also
clearing each node's Caddy storage). The bench's readiness check now waits for a
**stable streak** of successes (not a single 200) before measuring, so it won't
start against a half-up fleet. If followers never start serving TLS, that's a
JACO cross-host cert-propagation problem, not a workload issue.

## Local dry-run (no cluster)

The compose file builds and runs on a single host with stock `docker compose`:

```sh
cd tests/samples/jaco
REGISTRY=local docker compose build
docker compose up        # then map a port for web, or hit it on the compose network
```
