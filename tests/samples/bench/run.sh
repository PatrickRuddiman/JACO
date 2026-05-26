#!/usr/bin/env bash
# run.sh — run the benchmark against ONE stack and record a result.
#
#   ./run.sh <jaco|k8s|k3s|swarm> [--no-deploy] [--teardown] [--repeat N]
#
# Phases (only the deploy/teardown phases differ per stack — everything that
# produces a number is generic, which is what keeps the comparison unbiased):
#   1. deploy      — adapter brings up the cluster + workload   (timed)
#   2. wait-ready  — poll the ingress until it serves 2xx        (timed → TTL)
#   3. overhead    — snapshot idle node mem/load (control-plane footprint proxy)
#   4. warm-up     — drive throwaway load so caches/JITs reach steady state
#   5. load        — run the k6 scenario N times (--repeat), one sample each,
#                    monitoring load-generator CPU so it's never the bottleneck
#   6. collect     — adapter scrapes stack-internal metrics
#   7. score       — write results/<stack>-<ts>/result.json (per-sample samples
#                    + mean/stdev/95% CI, so single runs aren't over-read)
#
# Env (see lib/common.sh + loadgen/scenario.js): BENCH_PUBLIC_IPS,
# BENCH_PRIVATE_IPS or RESOURCE_GROUP; SSH_USER, SSH_KEY; BENCH_TARGET,
# BENCH_HOST_HEADER, BENCH_VUS, BENCH_DURATION, BENCH_RW_RATIO; BENCH_REPEAT
# (measurement samples, default 1), BENCH_WARMUP_DURATION (default 20s, "0" off).

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"

STACK="${1:-}"
[[ -n "$STACK" ]] || die "usage: run.sh <jaco|k8s|k3s|swarm> [--no-deploy] [--teardown] [--repeat N]"
shift || true
ADAPTER="$BENCH_DIR/adapters/$STACK.sh"
[[ -f "$ADAPTER" ]] || die "unknown stack '$STACK' (no adapters/$STACK.sh)"

DO_DEPLOY=1; DO_TEARDOWN=0
REPEAT="${BENCH_REPEAT:-1}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-deploy) DO_DEPLOY=0 ;;
    --teardown)  DO_TEARDOWN=1 ;;
    --repeat)    shift; REPEAT="${1:?--repeat needs a number}" ;;
    --repeat=*)  REPEAT="${1#*=}" ;;
    *) die "unknown flag $1" ;;
  esac
  shift
done
[[ "$REPEAT" =~ ^[0-9]+$ && "$REPEAT" -ge 1 ]] || die "--repeat must be a positive integer (got '$REPEAT')"

for dep in curl jq; do have "$dep" || die "$dep is required on the operator host"; done
have docker || have k6 || die "need docker (for grafana/k6) or a native k6 binary"

# shellcheck disable=SC1090
source "$ADAPTER"
resolve_nodes

TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUNDIR="$RESULTS_DIR/$STACK-$TS"
mkdir -p "$RUNDIR"
TARGET="$(bench_target)"
VUS="${BENCH_VUS:-20}"; DURATION="${BENCH_DURATION:-60s}"

log "stack=$STACK target=$TARGET vus=$VUS duration=$DURATION rundir=$RUNDIR"

# --- 1. deploy --------------------------------------------------------------
BOOTSTRAP_SECONDS=0
if [[ "$DO_DEPLOY" == "1" ]]; then
  t0="$(date +%s)"
  adapter_deploy
  BOOTSTRAP_SECONDS="$(( $(date +%s) - t0 ))"
  log "deploy complete in ${BOOTSTRAP_SECONDS}s"
fi

# --- 2. wait-ready (TTL) ----------------------------------------------------
log "waiting for ingress to serve traffic at $TARGET"
if TTL_SECONDS="$(wait_http_ready "$TARGET/healthz" "${BENCH_HOST_HEADER:-}" "${BENCH_READY_TIMEOUT:-420}")"; then
  log "ready in ${TTL_SECONDS}s"
else
  log "WARN: ingress not ready within timeout (${TTL_SECONDS}s); running load anyway"
fi

# --- one k6 run at a given duration, summary written under RUNDIR -----------
run_k6() {
  local duration="$1" summary="$2" x
  local kv=("BENCH_TARGET=$TARGET" "BENCH_VUS=$VUS" "BENCH_DURATION=$duration")
  [[ -n "${BENCH_HOST_HEADER:-}" ]] && kv+=("BENCH_HOST_HEADER=$BENCH_HOST_HEADER")
  [[ -n "${BENCH_RW_RATIO:-}" ]] && kv+=("BENCH_RW_RATIO=$BENCH_RW_RATIO")
  if have k6; then
    local a=(); for x in "${kv[@]}"; do a+=(--env "$x"); done
    k6 run "${a[@]}" "$BENCH_DIR/loadgen/scenario.js" --summary-export "$summary" || true
  else
    # Run as the host user so k6 can write the summary into the host-owned mount
    # (the grafana/k6 image otherwise runs as its own uid and is denied).
    local a=(); for x in "${kv[@]}"; do a+=(-e "$x"); done
    docker run --rm --user "$(id -u):$(id -g)" \
      -v "$BENCH_DIR/loadgen":/scenario:ro -v "$RUNDIR":/work \
      "${a[@]}" \
      grafana/k6 run /scenario/scenario.js --summary-export "/work/$(basename "$summary")" || true
  fi
}

# --- 3. idle overhead (control-plane footprint proxy, pre-load) -------------
log "snapshotting idle node overhead (control-plane footprint proxy)"
read -r OVH_MEM OVH_LOAD OVH_NODES < <(collect_idle_overhead) || { OVH_MEM=0; OVH_LOAD=0; OVH_NODES=0; }
[[ "$OVH_MEM"   =~ ^[0-9]+$   ]] || OVH_MEM=0
[[ "$OVH_LOAD"  =~ ^[0-9.]+$  ]] || OVH_LOAD=0
[[ "$OVH_NODES" =~ ^[0-9]+$   ]] || OVH_NODES=0
log "  idle: ${OVH_MEM}MB used + ${OVH_LOAD} loadavg across ${OVH_NODES} nodes"

# --- 4. warm-up (discarded — caches/JIT to steady state) --------------------
WARMUP="${BENCH_WARMUP_DURATION:-20s}"
if [[ "$WARMUP" != "0" && "$WARMUP" != "0s" ]]; then
  log "warm-up: $WARMUP of throwaway load"
  run_k6 "$WARMUP" "$RUNDIR/warmup-summary.json"
fi

# --- 5. load: N measured samples, monitoring load-generator CPU -------------
log "measuring: $REPEAT sample(s) of ${DURATION} at vus=$VUS"
CPU_SAMPLES=()
for i in $(seq 1 "$REPEAT"); do
  read -r idle0 tot0 < <(cpu_sample)
  run_k6 "$DURATION" "$RUNDIR/summary-$i.json"
  read -r idle1 tot1 < <(cpu_sample)
  [[ -f "$RUNDIR/summary-$i.json" ]] || die "k6 produced no summary for sample $i"
  cpu="$(awk -v di=$((idle1 - idle0)) -v dt=$((tot1 - tot0)) 'BEGIN{printf "%.1f", (dt>0)?(100*(1-di/dt)):0}')"
  CPU_SAMPLES+=("$cpu")
  log "  sample $i/$REPEAT done (load-gen host CPU ~${cpu}%)"
done
CPU_MAX="$(printf '%s\n' "${CPU_SAMPLES[@]}" | sort -rn | head -1)"
if awk -v c="${CPU_MAX:-0}" 'BEGIN{exit !(c>80)}'; then
  log "WARN: load-generator CPU peaked at ${CPU_MAX}% (>80%) — it may be the bottleneck, not the stack"
fi

# --- 6. collect -------------------------------------------------------------
adapter_collect "$RUNDIR" || log "WARN: collect step had errors"
LAG="$(cat "$RUNDIR/replica-lag-seconds.txt" 2>/dev/null || echo 0)"

# --- 7. score (write result.json: per-sample samples + mean/stdev/95% CI) ---
LABEL="$(adapter_label)"
LOC="$(setup_loc "$STACK")"
# Per-sample metrics from the k6 summaries (the warm-up summary is excluded by
# the summary-*.json glob). Slurp all samples into one array.
SAMPLES_JSON="$(jq -s '[ .[] | .metrics | {
    total: (.http_reqs.count // 0), rps: (.http_reqs.rate // 0),
    error_rate: (.http_req_failed.value // 0),
    avg: (.http_req_duration.avg // 0), p50: (.http_req_duration.med // 0),
    p95: (.http_req_duration["p(95)"] // 0), p99: (.http_req_duration["p(99)"] // 0),
    max: (.http_req_duration.max // 0)
  } ]' "$RUNDIR"/summary-*.json)"
CPUS_JSON="$(printf '%s\n' "${CPU_SAMPLES[@]}" | jq -Rn '[inputs | tonumber? // 0]')"

jq -n \
  --arg stack "$STACK" --arg label "$LABEL" --arg ts "$TS" --arg target "$TARGET" \
  --argjson bootstrap "$BOOTSTRAP_SECONDS" --argjson ttl "${TTL_SECONDS:-0}" \
  --arg vus "$VUS" --arg duration "$DURATION" --argjson repeat "$REPEAT" \
  --argjson lag "${LAG:-0}" --argjson loc "${LOC:-0}" \
  --argjson samples "$SAMPLES_JSON" --argjson cpus "$CPUS_JSON" \
  --argjson ovhmem "$OVH_MEM" --argjson ovhload "$OVH_LOAD" --argjson ovhnodes "$OVH_NODES" '
  def stats($a):
    ($a|length) as $n
    | (if $n==0 then 0 else (($a|add)/$n) end) as $mean
    | (if $n>1 then (($a|map((.-$mean)*(.-$mean))|add)/($n-1)) else 0 end) as $var
    | { mean:$mean, stdev:($var|sqrt),
        ci95:(if $n>1 then (1.96*($var|sqrt)/($n|sqrt)) else 0 end),
        min:(($a|min) // 0), max:(($a|max) // 0), n:$n };
  ($samples|map(.rps))         as $rps
  | ($samples|map(.p95))       as $p95
  | ($samples|map(.p99))       as $p99
  | ($samples|map(.error_rate)) as $err
  | {
    stack: $stack, label: $label, timestamp: $ts, target: $target,
    timings: { bootstrap_seconds: $bootstrap, ttl_seconds: $ttl },
    load: {
      vus: ($vus|tonumber), duration: $duration, repeat: $repeat,
      total_requests: (stats($samples|map(.total)).mean),
      rps: (stats($rps).mean),
      error_rate: (stats($err).mean),
      latency_ms: {
        avg: (stats($samples|map(.avg)).mean),
        p50: (stats($samples|map(.p50)).mean),
        p95: (stats($p95).mean),
        p99: (stats($p99).mean),
        max: (stats($samples|map(.max)).mean)
      },
      stats: { rps: stats($rps), p95: stats($p95), p99: stats($p99) },
      samples: $samples
    },
    load_generator: {
      cpu_pct_max: (($cpus|max) // 0),
      cpu_pct_avg: (if ($cpus|length)>0 then (($cpus|add)/($cpus|length)) else 0 end),
      saturated: ((($cpus|max) // 0) > 80)
    },
    overhead: { nodes: $ovhnodes, idle_mem_used_mb: $ovhmem, idle_loadavg: $ovhload },
    replica_lag_seconds: $lag,
    setup_loc: $loc
  }' >"$RUNDIR/result.json"

log "result written: $RUNDIR/result.json"
jq '{stack, timings, load: {repeat, rps, error_rate, latency_ms, stats}, load_generator, overhead, replica_lag_seconds, setup_loc}' "$RUNDIR/result.json"

# --- teardown (optional) ----------------------------------------------------
if [[ "$DO_TEARDOWN" == "1" ]]; then
  log "tearing down workload"
  adapter_teardown || true
fi

echo
log "done. Build the cross-stack comparison with: $BENCH_DIR/scorecard.sh"
