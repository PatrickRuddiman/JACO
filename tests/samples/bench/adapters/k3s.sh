#!/usr/bin/env bash
# k3s adapter for the bench harness. Same four functions as the JACO/Swarm
# adapters; relies on helpers + PUB/PRIV from lib/common.sh (sourced by run.sh).
#
# k3s runs its bundled Traefik with an ACME-staging cert resolver (see
# samples/k3s/manifests/00-traefik-acme.yaml), so bench it over HTTPS the same
# way: BENCH_TARGET=https://jaco.sh (k6 uses insecureSkipTLSVerify for staging).

adapter_label() { echo "k3s"; }

adapter_deploy() {
  log "k3s: installing k3s + deploying workload"
  BENCH_PUBLIC_IPS="${PUB[*]}" BENCH_PRIVATE_IPS="${PRIV[*]}" \
    "$SAMPLES_DIR/k3s/bootstrap/bootstrap.sh"
}

adapter_collect() {
  local out="$1" base host
  base="$(bench_target)"
  host="${BENCH_HOST_HEADER:-}"
  log "k3s: collecting internal metrics"

  # Scrape the api /metrics a few times through the ingress so the sample spans
  # multiple api pods (each keeps its own counters).
  local h=()
  [[ -n "$host" ]] && h=(-H "Host: $host")
  : >"$out/app-metrics.txt"
  for _ in $(seq 1 6); do
    curl -sk "${h[@]}" --max-time 5 "$base/api/metrics" >>"$out/app-metrics.txt" 2>/dev/null || true
    echo "# ---" >>"$out/app-metrics.txt"
  done

  # Cluster + rollout view from the server node.
  ssh_node "${PUB[0]}" "sudo k3s kubectl get nodes -o wide"           >"$out/k3s-nodes.txt"   2>&1 || true
  ssh_node "${PUB[0]}" "sudo k3s kubectl -n bench get pods -o wide"   >"$out/k3s-pods.txt"    2>&1 || true
  ssh_node "${PUB[0]}" "sudo k3s kubectl -n bench get deploy,svc,ing" >"$out/k3s-objects.txt" 2>&1 || true

  # Observed replication lag (max across scraped samples), for the scorecard —
  # same metric the JACO/Swarm adapters record, so the stacks are comparable.
  awk '/^bench_replica_lag_seconds/ {v=$2; if (v>m) m=v} END {print (m==""?0:m)}' \
    "$out/app-metrics.txt" >"$out/replica-lag-seconds.txt" 2>/dev/null || echo 0 >"$out/replica-lag-seconds.txt"
}

adapter_teardown() {
  log "k3s: deleting namespace 'bench'"
  ssh_node "${PUB[0]}" "sudo k3s kubectl delete namespace bench --wait=false >/dev/null 2>&1" || \
    log "k3s: namespace delete failed; tear the bed down with testbed/teardown.sh for a clean slate"
}
