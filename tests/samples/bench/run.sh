#!/usr/bin/env bash
# run.sh — run the benchmark against ONE stack and record a result.
#
#   ./run.sh <jaco|k8s|k3s|swarm> [--no-deploy] [--teardown]
#
# Phases (only the deploy/teardown phases differ per stack — everything that
# produces a number is generic, which is what keeps the comparison unbiased):
#   1. deploy      — adapter brings up the cluster + workload   (timed)
#   2. wait-ready  — poll the ingress until it serves 2xx        (timed → TTL)
#   3. load        — run the identical k6 scenario at the ingress
#   4. collect     — adapter scrapes stack-internal metrics
#   5. score       — write results/<stack>-<ts>/result.json
#
# Env (see lib/common.sh + loadgen/scenario.js): BENCH_PUBLIC_IPS,
# BENCH_PRIVATE_IPS or RESOURCE_GROUP; SSH_USER, SSH_KEY; BENCH_TARGET,
# BENCH_HOST_HEADER, BENCH_VUS, BENCH_DURATION, BENCH_RW_RATIO.

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"

STACK="${1:-}"
[[ -n "$STACK" ]] || die "usage: run.sh <jaco|k8s|k3s|swarm> [--no-deploy] [--teardown]"
shift || true
ADAPTER="$BENCH_DIR/adapters/$STACK.sh"
[[ -f "$ADAPTER" ]] || die "unknown stack '$STACK' (no adapters/$STACK.sh)"

DO_DEPLOY=1; DO_TEARDOWN=0
for a in "$@"; do
  case "$a" in
    --no-deploy) DO_DEPLOY=0 ;;
    --teardown)  DO_TEARDOWN=1 ;;
    *) die "unknown flag $a" ;;
  esac
done

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

# --- 3. load ----------------------------------------------------------------
log "running k6 load scenario"
# KV pairs shared by both invocation paths (native uses --env, docker uses -e).
KV=("BENCH_TARGET=$TARGET" "BENCH_VUS=$VUS" "BENCH_DURATION=$DURATION")
[[ -n "${BENCH_HOST_HEADER:-}" ]] && KV+=("BENCH_HOST_HEADER=$BENCH_HOST_HEADER")
[[ -n "${BENCH_RW_RATIO:-}" ]] && KV+=("BENCH_RW_RATIO=$BENCH_RW_RATIO")
if have k6; then
  nargs=(); for kv in "${KV[@]}"; do nargs+=(--env "$kv"); done
  k6 run "${nargs[@]}" "$BENCH_DIR/loadgen/scenario.js" --summary-export "$RUNDIR/summary.json" || true
else
  dargs=(); for kv in "${KV[@]}"; do dargs+=(-e "$kv"); done
  # Run as the host user so k6 can write summary.json into the host-owned mount
  # (the grafana/k6 image otherwise runs as its own uid and is denied).
  docker run --rm --user "$(id -u):$(id -g)" \
    -v "$BENCH_DIR/loadgen":/scenario:ro -v "$RUNDIR":/work \
    "${dargs[@]}" \
    grafana/k6 run /scenario/scenario.js --summary-export /work/summary.json || true
fi
[[ -f "$RUNDIR/summary.json" ]] || die "k6 produced no summary.json"

# --- 4. collect -------------------------------------------------------------
adapter_collect "$RUNDIR" || log "WARN: collect step had errors"
LAG="$(cat "$RUNDIR/replica-lag-seconds.txt" 2>/dev/null || echo 0)"

# --- 5. score (write result.json) ------------------------------------------
LABEL="$(adapter_label)"
LOC="$(setup_loc "$STACK")"
jq -n \
  --arg stack "$STACK" --arg label "$LABEL" --arg ts "$TS" --arg target "$TARGET" \
  --argjson bootstrap "$BOOTSTRAP_SECONDS" --argjson ttl "${TTL_SECONDS:-0}" \
  --arg vus "$VUS" --arg duration "$DURATION" \
  --argjson lag "${LAG:-0}" --argjson loc "${LOC:-0}" \
  --slurpfile s "$RUNDIR/summary.json" '
  ($s[0].metrics) as $m | {
    stack: $stack, label: $label, timestamp: $ts, target: $target,
    timings: { bootstrap_seconds: $bootstrap, ttl_seconds: $ttl },
    load: {
      vus: ($vus|tonumber), duration: $duration,
      total_requests: ($m.http_reqs.count // 0),
      rps: ($m.http_reqs.rate // 0),
      error_rate: ($m.http_req_failed.value // 0),
      latency_ms: {
        avg: ($m.http_req_duration.avg // 0),
        p50: ($m.http_req_duration.med // 0),
        p95: ($m.http_req_duration["p(95)"] // 0),
        p99: ($m.http_req_duration["p(99)"] // 0),
        max: ($m.http_req_duration.max // 0)
      }
    },
    replica_lag_seconds: $lag,
    setup_loc: $loc
  }' >"$RUNDIR/result.json"

log "result written: $RUNDIR/result.json"
jq . "$RUNDIR/result.json"

# --- teardown (optional) ----------------------------------------------------
if [[ "$DO_TEARDOWN" == "1" ]]; then
  log "tearing down workload"
  adapter_teardown || true
fi

echo
log "done. Build the cross-stack comparison with: $BENCH_DIR/scorecard.sh"
