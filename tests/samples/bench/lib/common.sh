#!/usr/bin/env bash
# Shared helpers for the bench harness. Sourced by run.sh, scorecard.sh, and the
# per-stack adapters. No side effects on source beyond defining functions/vars.

# --- paths ------------------------------------------------------------------
BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SAMPLES_DIR="$(cd "$BENCH_DIR/.." && pwd)"
REPO_ROOT="$(cd "$SAMPLES_DIR/../.." && pwd)"   # tests/samples -> repo root
TESTBED_DIR="$REPO_ROOT/tests/testbed"
RESULTS_DIR="$BENCH_DIR/results"

# --- ssh contract (shared with samples/<stack>/bootstrap) -------------------
SSH_USER="${SSH_USER:-azureuser}"
# Default to the per-bed key minted by the testbed deploy script; fall back to
# ~/.ssh/jaco for a bring-your-own key.
_bed_key="$TESTBED_DIR/.ssh/jaco"
SSH_KEY="${SSH_KEY:-$([ -f "$_bed_key" ] && echo "$_bed_key" || echo "$HOME/.ssh/jaco")}"
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o UserKnownHostsFile=/dev/null)

log()  { printf '[bench] %s\n' "$*" >&2; }
die()  { printf '[bench] ERROR: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

ssh_node() { ssh "${SSH_OPTS[@]}" "$SSH_USER@$1" "$2"; }

# Resolve node addressing into the PUB / PRIV arrays (node-1 first). Honors
# BENCH_PUBLIC_IPS / BENCH_PRIVATE_IPS, else queries Azure via RESOURCE_GROUP.
resolve_nodes() {
  read -r -a PUB <<<"${BENCH_PUBLIC_IPS:-}"
  read -r -a PRIV <<<"${BENCH_PRIVATE_IPS:-}"
  if [[ ${#PUB[@]} -eq 0 || ${#PRIV[@]} -eq 0 ]]; then
    [[ -n "${RESOURCE_GROUP:-}" ]] || die "set BENCH_PUBLIC_IPS+BENCH_PRIVATE_IPS or RESOURCE_GROUP"
    have az || die "az CLI required to resolve nodes from RESOURCE_GROUP"
    local prefix="${VM_NAME_PREFIX:-jaco}"
    mapfile -t PUB < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
      | jq -r --arg p "$prefix" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.publicIpAddresses[0].ipAddress')
    mapfile -t PRIV < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
      | jq -r --arg p "$prefix" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.privateIpAddresses[0]')
  fi
  [[ ${#PUB[@]} -ge 1 && ${#PUB[@]} -eq ${#PRIV[@]} ]] || die "could not resolve matching public/private node IPs"
  export PUB PRIV
}

# Poll an HTTP(S) endpoint until it returns 2xx/3xx or the timeout elapses.
# Echoes the elapsed seconds to ready (or the timeout on failure) and returns
# non-zero on timeout. Used to measure time-to-ready (TTL).
wait_http_ready() {
  local url="$1" host="${2:-}" timeout="${3:-300}"
  # Require N consecutive successes, not just one. The ingress LB round-robins
  # across every node, so a single 200 only proves ONE backend is up — a fresh
  # multi-node deploy can answer on the leader while followers are still warming
  # (e.g. TLS/cert propagation). Demanding a streak makes "ready" mean "the
  # whole fleet serves," so the bench doesn't start measuring a half-up cluster.
  local need="${BENCH_READY_STREAK:-5}"
  local start now code streak=0
  start="$(date +%s)"
  local curl_args=(-sk -o /dev/null -w '%{http_code}' --max-time 5)
  [[ -n "$host" ]] && curl_args+=(-H "Host: $host")
  while :; do
    now="$(date +%s)"
    (( now - start >= timeout )) && { echo "$timeout"; return 1; }
    code="$(curl "${curl_args[@]}" "$url" 2>/dev/null || echo 000)"
    if [[ "$code" =~ ^[23] ]]; then
      streak=$((streak + 1))
      (( streak >= need )) && { echo "$(( now - start ))"; return 0; }
    else
      streak=0
    fi
    sleep 2
  done
}

# Rough, automated "ease of setup" proxy: total lines across a stack's
# bootstrap scripts (fewer lines ⇒ easier). Manual 1–5 scores live in RUBRIC.md.
setup_loc() {
  local stack="$1"
  local dir="$SAMPLES_DIR/$stack/bootstrap"
  [[ -d "$dir" ]] || { echo 0; return; }
  find "$dir" -type f \( -name '*.sh' -o -name '*.yaml' -o -name '*.yml' \) -exec cat {} + 2>/dev/null | wc -l | tr -d ' '
}

# Default ingress target. The user's jaco.sh A record points at the LB IP; for
# stacks without TLS, set BENCH_TARGET=http://<lb-ip> and BENCH_HOST_HEADER.
bench_target() { echo "${BENCH_TARGET:-https://jaco.sh}"; }

# Snapshot the LOAD-GENERATOR host's CPU jiffies from /proc/stat. Echoes
# "<idle> <total>". Two snapshots around a load run give the average host-CPU
# busy %, used to verify the generator isn't the bottleneck (a top benchmarking
# pitfall: a saturated load generator caps offered load and is mistaken for the
# system-under-test's ceiling). Returns "0 0" on non-Linux so callers degrade.
cpu_sample() {
  awk '/^cpu /{idle=$5+$6; total=0; for(i=2;i<=NF;i++) total+=$i; print idle, total; exit}' \
    /proc/stat 2>/dev/null || echo "0 0"
}

# Idle control-plane / per-node overhead proxy. With the workload identical and
# idle across stacks, the cross-stack delta in node memory + load average ≈ the
# orchestrator's fixed control-plane cost (etcd/apiserver vs raft vs swarm
# managers). Measured post-deploy, pre-load. Echoes "<used_mb_total> <loadavg_total> <nodes>".
# This is a coarse but fair, automatable proxy — not per-process attribution.
collect_idle_overhead() {
  local total_mb=0 total_load="0" n=0 out mb ld
  for ip in "${PUB[@]}"; do
    out="$(ssh_node "$ip" "echo \"\$(free -m | awk '/^Mem:/{print \$3}') \$(awk '{print \$1}' /proc/loadavg)\"" 2>/dev/null || echo "0 0")"
    mb="${out%% *}"; ld="${out##* }"
    [[ "$mb" =~ ^[0-9]+$ ]] || mb=0
    [[ "$ld" =~ ^[0-9.]+$ ]] || ld=0
    total_mb=$((total_mb + mb))
    total_load="$(awk -v a="$total_load" -v b="$ld" 'BEGIN{printf "%.2f", a+b}')"
    n=$((n + 1))
  done
  echo "$total_mb $total_load $n"
}
