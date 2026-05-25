# k3s — follow-up pass

**Status: stubbed.** The shared [workload](../workload) and the
[bench harness](../bench) are in place; this stack's bootstrap + manifests land
in the follow-up.

## Planned bootstrap (from the minimal Debian baseline)

1. node-1 (server): `curl -sfL https://get.k3s.io | sh -` → grab
   `/var/lib/rancher/k3s/server/node-token`.
2. node-2/3 (agents):
   `curl -sfL https://get.k3s.io | K3S_URL=https://<node1-priv>:6443 K3S_TOKEN=<token> sh -`.
3. k3s ships Traefik as the ingress controller and a service LB already bound to
   host 80/443, so it sits behind the testbed LB with no extra install.
4. `kubectl apply -f manifests/` (k3s bundles kubectl).

## Planned `manifests/`

Same shape as the [k8s manifests](../k8s) (Deployments + Services for
`redis-primary`/`redis-replica`/`api`/`web`, matching resource limits), but
using a k3s `IngressRoute`/`Ingress` for `web:80`. The same CoreDNS resolver
note as k8s applies to the `web` nginx config.

The bench adapter (`../bench/adapters/k3s.sh`) will wrap this so
`../bench/run.sh k3s` works end-to-end. k3s is the natural low-overhead
counterpoint to full kubeadm in the comparison.
