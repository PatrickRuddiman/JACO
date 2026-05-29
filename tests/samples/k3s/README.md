# k3s — bench stack

Deploys the shared [workload](../workload) on [k3s](https://k3s.io), for the
comparative benchmark ([#51](https://github.com/PatrickRuddiman/JACO/issues/51)).
k3s is the lightweight-Kubernetes counterpoint in the comparison: a single
binary, bundled Traefik ingress + servicelb, no separate control-plane install.
The images and deployment shape are byte-identical to the [JACO](../jaco) and
[Swarm](../swarm) samples — only the orchestration differs.

## Files

```
k3s/
├── manifests/
│   ├── 00-traefik-acme.yaml   # HelmChartConfig: ACME-staging cert resolver for Traefik
│   ├── 05-namespace.yaml      # the `bench` namespace
│   ├── 10-redis.yaml          # redis primary (1) + replica (2) Deployments + Services
│   ├── 20-postgres.yaml       # pg primary (jaco-2) + replica (jaco-3), pinned via nodeSelector
│   ├── 30-api.yaml            # api Deployment (2) + Service
│   ├── 40-web.yaml            # web Deployment (2) + Service
│   └── 50-ingress.yaml        # Traefik Ingress: jaco.sh → web:80, TLS via the resolver
└── bootstrap/
    ├── bootstrap.sh           # operator-side: prep + registry + build/push + k3s server/agents + apply
    └── prepare-node.sh        # runs on each node: cloud-init wait + containerd registry config
```

## One-shot

From the operator host, with the testbed deployed:

```sh
export BENCH_PUBLIC_IPS="<n1-pub> <n2-pub> <n3-pub>"   # node-1 first = the k3s server
export BENCH_PRIVATE_IPS="<n1-priv> <n2-priv> <n3-priv>"
tests/samples/k3s/bootstrap/bootstrap.sh
```

In order: prepares every node (cloud-init wait + `registries.yaml` so containerd
pulls the in-cluster registry over HTTP); installs Docker + `registry:2` on
node-1 and **builds the workload images there** (`bench-web` / `bench-api` /
`bench-postgres` → `<node-1-private-ip>:5000`); installs the **k3s server** on
node-1 and joins node-2/3 as **agents** with the node-token; rewrites the
registry into the manifests and `kubectl apply`s them.

## How it maps to the JACO deployment

| concern | JACO | k3s |
|---------|------|-----|
| replicas / limits | `jaco.yaml` + compose `deploy.resources` | Deployment `replicas` + `resources.limits` |
| service discovery | per-deployment bridge + embedded DNS | ClusterIP Services + CoreDNS (`api` resolves in-namespace — web image unchanged) |
| pg primary/replica on different nodes | `placement: hosts [jaco-2]/[jaco-3]` | `nodeSelector: kubernetes.io/hostname: jaco-2 / jaco-3` |
| north-south ingress | Caddy route → web (TLS) | bundled Traefik Ingress → web (TLS via ACME-staging resolver), servicelb on :80/:443 |

## Ingress / TLS

k3s ships **Traefik** + **servicelb** (klipper), already bound to host `:80`/`:443`
on every node — so it sits behind the testbed LB with no extra install. The
[`00-traefik-acme.yaml`](manifests/00-traefik-acme.yaml) `HelmChartConfig` patches
Traefik with a **Let's Encrypt staging** cert resolver (`lestaging`), and the
[Ingress](manifests/50-ingress.yaml) requests TLS for `jaco.sh` from it. So k3s is
benched over **HTTPS**, same target as JACO/Swarm (per [RUBRIC.md](../bench/RUBRIC.md)):

```sh
cd tests/samples/bench
export BENCH_PUBLIC_IPS="..." BENCH_PRIVATE_IPS="..."
export BENCH_TARGET="https://jaco.sh"
./run.sh k3s
./collect.sh
```

> **Ingress note:** unlike Swarm (where we added Caddy to match JACO), k3s is
> benched with its *native* Traefik — representative of how k3s is actually run.
> The terminator therefore differs from JACO/Swarm (Traefik vs Caddy), so a small
> ingress-attributable latency delta can't be fully excluded; the orchestration
> data plane (scheduling, service DNS, cross-node replication) is what's held
> constant. Staging certs aren't browser-trusted, so k6 uses `insecureSkipTLSVerify`.

## Verify

```sh
ssh azureuser@<n1-pub> 'sudo k3s kubectl -n bench get pods -o wide'   # all pods Running, pg on jaco-2/3
curl -sk https://jaco.sh/api/notes      # JSON list (reads a redis replica)
curl -sk https://jaco.sh/api/metrics    # Prometheus metrics (incl. pg replica lag)
```

> The `kubernetes.io/hostname` selectors assume the default `jaco-1/2/3` node
> names — adjust in `20-postgres.yaml` if you deploy under a different prefix.
