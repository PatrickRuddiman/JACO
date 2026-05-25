# Kubernetes (kubeadm) — follow-up pass

**Status: stubbed.** The shared [workload](../workload) and the
[bench harness](../bench) are in place; this stack's bootstrap + manifests land
in the follow-up.

## Planned bootstrap (from the minimal Debian baseline)

1. Install containerd + kubeadm/kubelet/kubectl on all three nodes.
2. `kubeadm init` on node-1; install a CNI (e.g. Cilium or Calico).
3. `kubeadm join` node-2 and node-3 with the printed token.
4. Install an ingress controller (ingress-nginx) bound to host 80/443 so it
   sits behind the testbed LB.
5. `kubectl apply -f manifests/` — see below.

## Planned `manifests/`

- `redis-primary` Deployment (1) + Service `redis-primary`.
- `redis-replica` Deployment (2) + Service `redis-replica`, args
  `--replicaof redis-primary 6379 --replica-read-only yes`.
- `api` Deployment (2) + Service, env `REDIS_PRIMARY` / `REDIS_REPLICA`,
  resource limits matching the JACO compose (0.5 cpu / 256M).
- `web` Deployment (2) + Service.
- `Ingress` routing the bench host to `web:80`.

The `nginx.conf` `resolver 127.0.0.11` (docker embedded DNS) must be swapped
for the cluster DNS (`kube-dns`/CoreDNS service IP) in a k8s-specific web image
or ConfigMap — the one place the workload is not byte-identical across stacks.

The bench adapter (`../bench/adapters/k8s.sh`) will wrap this so
`../bench/run.sh k8s` works end-to-end.
