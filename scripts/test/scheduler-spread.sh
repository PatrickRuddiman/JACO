#!/usr/bin/env bash
# scheduler-spread.sh — E2E: 3-node cluster, apply a 3-replica
# SPREAD-placement service, assert each replica lands on a distinct
# host.
#
# Gated by JACO_SCHEDULER_SPREAD_FORCE=1.

set -euo pipefail

if [[ "${JACO_SCHEDULER_SPREAD_FORCE:-0}" != "1" ]]; then
  echo "SKIP scheduler-spread.sh: set JACO_SCHEDULER_SPREAD_FORCE=1 to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-spread-XXXX)"
declare -a PIDS=()
trap 'for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done; rm -rf "$WORK"' EXIT

go build -o "$WORK/jacod" ./cmd/jacod
go build -o "$WORK/jaco"  ./cmd/jaco

start_node() {
  local n=$1 listen=$2 cluster=$3
  mkdir -p "$WORK/data-$n"
  cat > "$WORK/jacod-$n.yaml" <<EOF
data_dir: $WORK/data-$n
listen_addr: 127.0.0.1:$listen
cluster_addr: 127.0.0.1:$cluster
unix_socket: $WORK/jaco-$n.sock
wg_port: 5182$n
log_level: info
ipam_pool: 10.244.0.0/16
EOF
  JACO_CONFIG="$WORK/jacod-$n.yaml" "$WORK/jacod" >"$WORK/jacod-$n.log" 2>&1 &
  PIDS+=("$!")
}
start_node 1 28300 28310
start_node 2 28400 28410
start_node 3 28500 28510
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco-1.sock" --name spread 2>&1 | awk '/operator_token:/ {print $2}')
sleep 1
for sock in jaco-2 jaco-3; do
  JOIN=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:28300 2>&1 | awk '/^Join token:/ {print $3}')
  "$WORK/jaco" node join --socket "$WORK/$sock.sock" --peer 127.0.0.1:28300 --token "$JOIN"
done
sleep 3

cat > "$WORK/jaco.yaml" <<'EOF'
deployment: spread
services:
  - name: web
    compose_service: web
    replicas: 3
    placement: spread
EOF
cat > "$WORK/compose.yml" <<'EOF'
services:
  web: { image: nginx:1.27 }
EOF
JACO_TOKEN="$TOKEN" "$WORK/jaco" apply "$WORK/jaco.yaml" --server 127.0.0.1:28300 --compose "$WORK/compose.yml"
sleep 3

# Walk the leader's state via `jaco status` for the SPREAD assertion.
STATUS=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" status --server 127.0.0.1:28300 spread 2>&1)
DISTINCT=$(echo "$STATUS" | awk '/spread-web-/ {print $3}' | sort -u | wc -l)
if [[ "$DISTINCT" -lt 3 ]]; then
  echo "FAIL: $DISTINCT distinct hosts; want 3 (SPREAD across the cluster)"
  echo "$STATUS"
  exit 1
fi
echo "PASS: scheduler-spread (3 replicas across 3 hosts)"
