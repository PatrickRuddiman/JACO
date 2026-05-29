#!/usr/bin/env bash
# Kubernetes (kubeadm) adapter for the bench harness. Same four functions as the
# JACO/Swarm/k3s adapters; relies on helpers + PUB/PRIV from lib/common.sh
# (sourced by run.sh).
#
# kubeadm has no bundled ingress, so the sample installs ingress-nginx +
# cert-manager with a Let's Encrypt staging issuer (see samples/k8s/manifests +
# bootstrap). Bench over HTTPS the same way: BENCH_TARGET=https://jaco.sh (k6
# uses insecureSkipTLSVerify for the staging cert).

KUBECTL="sudo kubectl --kubeconfig /etc/kubernetes/admin.conf"

adapter_label() { echo "Kubernetes (kubeadm)"; }

adapter_deploy() {
  log "k8s: kubeadm init + deploying workload"
  BENCH_PUBLIC_IPS="${PUB[*]}" BENCH_PRIVATE_IPS="${PRIV[*]}" \
    "$SAMPLES_DIR/k8s/bootstrap/bootstrap.sh"
}

adapter_collect() {
  local out="$1" base host
  base="$(bench_target)"
  host="${BENCH_HOST_HEADER:-}"
  log "k8s: collecting internal metrics"

  # Scrape the api /metrics a few times through the ingress so the sample spans
  # multiple api pods (each keeps its own counters).
  local h=()
  [[ -n "$host" ]] && h=(-H "Host: $host")
  : >"$out/app-metrics.txt"
  for _ in $(seq 1 6); do
    curl -sk "${h[@]}" --max-time 5 "$base/api/metrics" >>"$out/app-metrics.txt" 2>/dev/null || true
    echo "# ---" >>"$out/app-metrics.txt"
  done

  # Cluster + rollout view from the control-plane node.
  ssh_node "${PUB[0]}" "$KUBECTL get nodes -o wide"           >"$out/k8s-nodes.txt"   2>&1 || true
  ssh_node "${PUB[0]}" "$KUBECTL -n bench get pods -o wide"   >"$out/k8s-pods.txt"    2>&1 || true
  ssh_node "${PUB[0]}" "$KUBECTL -n bench get deploy,svc,ing" >"$out/k8s-objects.txt" 2>&1 || true

  # Observed replication lag (max across scraped samples), for the scorecard —
  # same metric the other adapters record, so the stacks are comparable.
  awk '/^bench_replica_lag_seconds/ {v=$2; if (v>m) m=v} END {print (m==""?0:m)}' \
    "$out/app-metrics.txt" >"$out/replica-lag-seconds.txt" 2>/dev/null || echo 0 >"$out/replica-lag-seconds.txt"
}

adapter_teardown() {
  log "k8s: deleting namespace 'bench'"
  ssh_node "${PUB[0]}" "$KUBECTL delete namespace bench --wait=false >/dev/null 2>&1" || \
    log "k8s: namespace delete failed; tear the bed down with testbed/teardown.sh for a clean slate"
}
