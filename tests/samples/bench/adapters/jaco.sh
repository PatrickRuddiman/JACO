#!/usr/bin/env bash
# JACO adapter for the bench harness. Defines the four functions run.sh calls:
#   adapter_deploy    — bootstrap the cluster + deploy the workload
#   adapter_collect   — scrape stack-internal metrics into the run dir
#   adapter_teardown  — remove the workload (cluster left running)
#   adapter_label     — human label for the scorecard
# Relies on helpers + PUB/PRIV from lib/common.sh (already sourced by run.sh).

adapter_label() { echo "JACO"; }

adapter_deploy() {
  log "JACO: bootstrapping cluster + deploying workload"
  BENCH_PUBLIC_IPS="${PUB[*]}" BENCH_PRIVATE_IPS="${PRIV[*]}" \
    "$SAMPLES_DIR/jaco/bootstrap/bootstrap.sh"
}

adapter_collect() {
  local out="$1" base host
  base="$(bench_target)"
  host="${BENCH_HOST_HEADER:-}"
  log "JACO: collecting internal metrics"

  # Scrape the api /metrics a few times through the ingress so the sample
  # spans multiple api replicas (each replica keeps its own counters).
  local h=()
  [[ -n "$host" ]] && h=(-H "Host: $host")
  : >"$out/app-metrics.txt"
  for _ in $(seq 1 6); do
    curl -sk "${h[@]}" --max-time 5 "$base/api/metrics" >>"$out/app-metrics.txt" 2>/dev/null || true
    echo "# ---" >>"$out/app-metrics.txt"
  done

  # Cluster + rollout view from node-1.
  ssh_node "${PUB[0]}" "sudo jaco status"        >"$out/jaco-status.txt"  2>&1 || true
  ssh_node "${PUB[0]}" "sudo jaco cluster status" >"$out/jaco-cluster.txt" 2>&1 || true

  # Observed replication lag (max across scraped replicas), for the scorecard.
  awk '/^bench_replica_lag_seconds/ {v=$2; if (v>m) m=v} END {print (m==""?0:m)}' \
    "$out/app-metrics.txt" >"$out/replica-lag-seconds.txt" 2>/dev/null || echo 0 >"$out/replica-lag-seconds.txt"
}

adapter_teardown() {
  log "JACO: removing workload deployment 'bench'"
  # `jaco delete` still requires an operator token even on-node; best-effort.
  ssh_node "${PUB[0]}" "sudo jaco delete bench >/dev/null 2>&1" || \
    log "JACO: 'jaco delete bench' needs --server/--token; tear the bed down with testbed/teardown.sh for a clean slate"
}
