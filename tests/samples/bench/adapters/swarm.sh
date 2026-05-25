#!/usr/bin/env bash
# Docker Swarm adapter for the bench harness. Same four functions as the JACO
# adapter; relies on helpers + PUB/PRIV from lib/common.sh (sourced by run.sh).
#
# Swarm runs its own Caddy ingress (TLS for jaco.sh, ACME staging) published via
# the routing mesh, mirroring JACO — so bench it over HTTPS the same way:
# BENCH_TARGET=https://jaco.sh (k6 uses insecureSkipTLSVerify for the staging cert).

adapter_label() { echo "Docker Swarm"; }

adapter_deploy() {
  log "Swarm: forming swarm + deploying stack"
  BENCH_PUBLIC_IPS="${PUB[*]}" BENCH_PRIVATE_IPS="${PRIV[*]}" \
    "$SAMPLES_DIR/swarm/bootstrap/bootstrap.sh"
}

adapter_collect() {
  local out="$1" base host
  base="$(bench_target)"
  host="${BENCH_HOST_HEADER:-}"
  log "Swarm: collecting internal metrics"

  # Scrape the api /metrics a few times through the ingress so the sample spans
  # multiple api tasks (each keeps its own counters).
  local h=()
  [[ -n "$host" ]] && h=(-H "Host: $host")
  : >"$out/app-metrics.txt"
  for _ in $(seq 1 6); do
    curl -sk "${h[@]}" --max-time 5 "$base/api/metrics" >>"$out/app-metrics.txt" 2>/dev/null || true
    echo "# ---" >>"$out/app-metrics.txt"
  done

  # Stack + node view from the manager (node-1).
  ssh_node "${PUB[0]}" "sudo docker stack services bench"      >"$out/swarm-services.txt" 2>&1 || true
  ssh_node "${PUB[0]}" "sudo docker stack ps bench --no-trunc" >"$out/swarm-ps.txt"       2>&1 || true
  ssh_node "${PUB[0]}" "sudo docker node ls"                   >"$out/swarm-nodes.txt"    2>&1 || true

  # Observed replication lag (max across scraped samples), for the scorecard —
  # same metric the JACO adapter records, so the stacks are comparable.
  awk '/^bench_replica_lag_seconds/ {v=$2; if (v>m) m=v} END {print (m==""?0:m)}' \
    "$out/app-metrics.txt" >"$out/replica-lag-seconds.txt" 2>/dev/null || echo 0 >"$out/replica-lag-seconds.txt"
}

adapter_teardown() {
  log "Swarm: removing stack 'bench'"
  ssh_node "${PUB[0]}" "sudo docker stack rm bench >/dev/null 2>&1" || \
    log "Swarm: 'docker stack rm bench' failed; tear the bed down with testbed/teardown.sh for a clean slate"
}
