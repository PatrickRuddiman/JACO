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
          raw: { rps:$r.load.rps, ttl_s:$r.timings.ttl_seconds, p95_ms:$r.load.latency_ms.p95,
                 err:$r.load.error_rate, lag_s:$r.replica_lag_seconds, setup_loc:$r.setup_loc }
        })
    | sort_by(-.composite)
  ' "${files[@]}" >"$RESULTS_DIR/scorecard.json"

log "wrote $RESULTS_DIR/scorecard.json"
echo
printf 'stack\tcomposite\tsetup\tttl\trps\tlatency\treliab\trepl\t|\trps\tttl_s\tp95_ms\terr\n'
jq -r '.[] | [
    .label,
    (.composite|.*10|round/10),
    (.scores.setup|round), (.scores.ttl|round), (.scores.throughput|round),
    (.scores.latency|round), (.scores.reliability|round), (.scores.replication|round),
    "|",
    (.raw.rps|.*10|round/10), .raw.ttl_s, (.raw.p95_ms|.*10|round/10),
    (.raw.err|.*1000|round/1000)
  ] | @tsv' "$RESULTS_DIR/scorecard.json" \
  | { command -v column >/dev/null 2>&1 && column -t -s$'\t' || cat; }
echo
echo "(scores 0–100, higher better; best stack per dimension = 100. raw columns are actuals.)"
