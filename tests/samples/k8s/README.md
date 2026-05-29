# Kubernetes (kubeadm) — bench stack

Deploys the shared [workload](../workload) on a **kubeadm**-built Kubernetes
cluster, for the comparative benchmark
([#51](https://github.com/PatrickRuddiman/JACO/issues/51)). This is the
"full, do-it-yourself Kubernetes" end of the spectrum — you install the runtime,
the control plane, a CNI, and an ingress controller yourself. The workload
manifests are byte-identical to the [k3s sample](../k3s); only the bootstrap and
the ingress/TLS plumbing differ.

## Files

```
k8s/
├── manifests/
│   ├── 05-namespace.yaml      # the `bench` namespace
│   ├── 10-redis.yaml          # redis primary (1) + replica (2) Deployments + Services
│   ├── 20-postgres.yaml       # pg primary (jaco-2) + replica (jaco-3), pinned via nodeSelector
│   ├── 30-api.yaml            # api Deployment (2) + Service
│   ├── 40-web.yaml            # web Deployment (2) + Service
│   ├── 50-clusterissuer.yaml  # cert-manager ClusterIssuer (Let's Encrypt staging)
│   └── 60-ingress.yaml        # ingress-nginx Ingress: jaco.sh → web:80, TLS via cert-manager
└── bootstrap/
    ├── bootstrap.sh           # operator-side: prep + registry + build/push + kubeadm init/join + CNI + Helm + apply
    └── prepare-node.sh        # runs on each node: containerd + kubeadm toolchain + kernel/sysctl prereqs
```

## One-shot

From the operator host, with the testbed deployed:

```sh
export BENCH_PUBLIC_IPS="<n1-pub> <n2-pub> <n3-pub>"   # node-1 first = the control-plane
export BENCH_PRIVATE_IPS="<n1-priv> <n2-priv> <n3-priv>"
tests/samples/k8s/bootstrap/bootstrap.sh
```

In order: prepares every node (containerd from the Docker repo + an insecure
HTTP mirror for the in-cluster registry, kubeadm/kubelet/kubectl, kernel/sysctl
prereqs); installs Docker + `registry:2` on node-1 and **builds the workload
images there**; `kubeadm init` on node-1, installs the **flannel** CNI, joins
node-2/3 as workers; installs **ingress-nginx** (hostNetwork DaemonSet) and
**cert-manager** via Helm; then `kubectl apply`s the manifests.

## How it maps to the JACO deployment

| concern | JACO | Kubernetes (kubeadm) |
|---------|------|----------------------|
| replicas / limits | `jaco.yaml` + compose `deploy.resources` | Deployment `replicas` + `resources.limits` |
| service discovery | per-deployment bridge + embedded DNS | ClusterIP Services + CoreDNS (`api` resolves in-namespace — web image unchanged) |
| pg primary/replica on different nodes | `placement: hosts [jaco-2]/[jaco-3]` | `nodeSelector: kubernetes.io/hostname: jaco-2 / jaco-3` |
| north-south ingress | Caddy route → web (TLS) | ingress-nginx (hostNetwork DaemonSet) → web, TLS via cert-manager (ACME staging) |

## Ingress / TLS

kubeadm ships **no** ingress controller, so the sample installs **ingress-nginx**
(as a `hostNetwork` DaemonSet, binding `:80`/`:443` on every node behind the
testbed LB) and **cert-manager** with a Let's Encrypt **staging** `ClusterIssuer`.
The [Ingress](manifests/60-ingress.yaml) is annotated with the issuer + a `tls`
block, so cert-manager obtains the staging cert for `jaco.sh` (HTTP-01 via
ingress-nginx) and ingress-nginx serves it. Benched over **HTTPS**, same target
as the other stacks (per [RUBRIC.md](../bench/RUBRIC.md)):

```sh
cd tests/samples/bench
export BENCH_PUBLIC_IPS="..." BENCH_PRIVATE_IPS="..."
export BENCH_TARGET="https://jaco.sh"
./run.sh k8s
./collect.sh
```

> **Ingress note:** like Swarm, kubeadm has no native ingress to lean on — but
> the idiomatic kubeadm choice is ingress-nginx + cert-manager (not Caddy), so
> that's what the sample installs. It's a *different* terminator than JACO/Swarm
> (and the install cost is part of kubeadm's "ease of setup" story, which the
> benchmark intentionally captures). Staging certs aren't browser-trusted, so k6
> uses `insecureSkipTLSVerify`.

## Verify

```sh
ssh azureuser@<n1-pub> 'sudo kubectl -n bench get pods -o wide'   # all pods Running, pg on jaco-2/3
curl -sk https://jaco.sh/api/notes      # JSON list (reads a redis replica)
curl -sk https://jaco.sh/api/metrics    # Prometheus metrics (incl. pg replica lag)
```

> The `kubernetes.io/hostname` selectors assume the default `jaco-1/2/3` node
> names — adjust in `20-postgres.yaml` if you deploy under a different prefix.
> kubeadm's control-plane preflight requires **≥2 vCPU**; if the testbed VM SKU
> is smaller, bump it in `tests/testbed/parameters.bicepparam`.
