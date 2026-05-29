#!/usr/bin/env bash
# scorecard.sh — build the cross-stack comparison from the latest result.json
# per stack. Normalizes each rubric dimension to 0–100 (best stack = 100) and
# applies the weights below. Writes results/scorecard.json and prints a table.
#
# Weights (see RUBRIC.md). Override any with env, e.g. W_RPS=0.30.
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"

W_SETUP="${W_SETUP:-0.20}"   # ease of setup (bootstrap LOC proxy, lower better)
W_TTL="${W_TTL:-0.20}"       # time-to-ready (lower better)
W_RPS="${W_RPS:-0.25}"       # throughput (higher better)
W_LAT="${W_LAT:-0.20}"       # p95 latency (lower better)
W_REL="${W_REL:-0.10}"       # reliability = 1 - error_rate
W_LAG="${W_LAG:-0.05}"       # replication lag (lower better)

shopt -s nullglob
files=("$RESULTS_DIR"/*/result.json)
[[ ${#files[@]} -gt 0 ]] || die "no results yet — run ./run.sh <stack> first"

jq -s \
  --argjson w "{\"setup\":$W_SETUP,\"ttl\":$W_TTL,\"rps\":$W_RPS,\"lat\":$W_LAT,\"rel\":$W_REL,\"lag\":$W_LAG}" '
  (group_by(.stack) | map(max_by(.timestamp))) as $rows
  | ($rows | map(.load.rps) | max) as $maxRps
  | ($rows | map(.timings.ttl_seconds)      | map(select(.>0)) | min) as $minTtl
  | ($rows | map(.load.latency_ms.p95)      | map(select(.>0)) | min) as $minP95
  | ($rows | map(.replica_lag_seconds)      | map(select(.>0)) | min) as $minLag
  | ($rows | map(.setup_loc)                | map(select(.>0)) | min) as $minLoc
  | def lower(v; m): (if v<=0 then 100 elif (m==null) then 100 else (m/v*100) end);
    $rows | map(
      . as $r
      | (if $maxRps>0 then $r.load.rps/$maxRps*100 else 0 end)          as $sRps
      | lower($r.timings.ttl_seconds; $minTtl)                          as $sTtl
      | lower($r.load.latency_ms.p95; $minP95)                          as $sLat
      | ((1 - $r.load.error_rate)*100)                                  as $sRel
      | lower($r.replica_lag_seconds; $minLag)                          as $sLag
      | (if $r.setup_loc<=0 then 0 else lower($r.setup_loc; $minLoc) end) as $sSetup
      | {
          stack: $r.stack, label: $r.label,
          composite: (($sSetup*$w.setup)+($sTtl*$w.ttl)+($sRps*$w.rps)+($sLat*$w.lat)+($sRel*$w.rel)+($sLag*$w.lag)),
          scores: { setup:$sSetup, ttl:$sTtl, throughput:$sRps, latency:$sLat, reliability:$sRel, replication:$sLag },
          raw: { rps:$r.load.rps, rps_ci:($r.load.stats.rps.ci95 // 0), n:($r.load.repeat // 1),
                 ttl_s:$r.timings.ttl_seconds, p95_ms:$r.load.latency_ms.p95,
                 err:$r.load.error_rate, lag_s:$r.replica_lag_seconds, setup_loc:$r.setup_loc,
                 ovh_mem:($r.overhead.idle_mem_used_mb // 0), cpu:($r.load_generator.cpu_pct_max // 0),
                 saturated:($r.load_generator.saturated // false) }
        })
    | sort_by(-.composite)
  ' "${files[@]}" >"$RESULTS_DIR/scorecard.json"

log "wrote $RESULTS_DIR/scorecard.json"
echo
printf 'stack\tcomposite\tsetup\tttl\trps\tlatency\treliab\trepl\t|\trps\t±95\tn\tttl_s\tp95_ms\terr\tovh_mb\tlg_cpu%%\n'
jq -r '.[] | [
    .label,
    (.composite|.*10|round/10),
    (.scores.setup|round), (.scores.ttl|round), (.scores.throughput|round),
    (.scores.latency|round), (.scores.reliability|round), (.scores.replication|round),
    "|",
    (.raw.rps|.*10|round/10), (.raw.rps_ci|.*10|round/10), .raw.n,
    .raw.ttl_s, (.raw.p95_ms|.*10|round/10),
    (.raw.err|.*1000|round/1000),
    .raw.ovh_mem, (.raw.cpu|.*10|round/10)
  ] | @tsv' "$RESULTS_DIR/scorecard.json" \
  | { command -v column >/dev/null 2>&1 && column -t -s$'\t' || cat; }
echo
echo "(scores 0–100, higher better; best stack per dimension = 100. raw columns are actuals.)"
echo "(rps is the mean of n samples; ±95 is the 95% CI half-width — overlapping intervals ⇒ a statistical tie."
echo " ovh_mb = idle node memory used across the cluster, a control-plane footprint proxy; lg_cpu% = peak load-generator CPU.)"
# Flag any run whose load generator may have been the bottleneck.
if jq -e 'any(.[]; .raw.saturated)' "$RESULTS_DIR/scorecard.json" >/dev/null 2>&1; then
  echo
  jq -r '.[] | select(.raw.saturated) | "WARNING: \(.label) load-generator CPU peaked >80% (\(.raw.cpu|.*10|round/10)%) — its throughput may reflect the generator, not the stack."' \
    "$RESULTS_DIR/scorecard.json"
fi
