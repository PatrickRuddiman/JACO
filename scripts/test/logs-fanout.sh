#!/usr/bin/env bash
# logs-fanout.sh — E2E: 2-node cluster, apply a 2-replica deployment,
# `jaco logs --follow` against the leader, assert lines from both
# replicas appear in the merged stream within the timeout.
#
# Gated by JACO_LOGS_FANOUT_FORCE=1. Needs docker reachable on both
# fake "nodes" (same host, two jacod instances).

set -euo pipefail

if [[ "${JACO_LOGS_FANOUT_FORCE:-0}" != "1" ]]; then
  echo "SKIP logs-fanout.sh: set JACO_LOGS_FANOUT_FORCE=1 to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-logs-fanout-XXXX)"
trap 'kill $JACOD1_PID 2>/dev/null || true; kill $JACOD2_PID 2>/dev/null || true; kill $LOGS_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

go build -o "$WORK/jacod" ./cmd/jacod
go build -o "$WORK/jaco"  ./cmd/jaco

mkconfig() {
  local n=$1 listen=$2 cluster=$3
  mkdir -p "$WORK/data-$n"
  cat > "$WORK/jacod-$n.yaml" <<EOF
data_dir: $WORK/data-$n
listen_addr: 127.0.0.1:$listen
cluster_addr: 127.0.0.1:$cluster
unix_socket: $WORK/jaco-$n.sock
wg_port: 51820
log_level: info
ipam_pool: 10.244.0.0/16
EOF
}
mkconfig 1 27400 27401
mkconfig 2 27500 27501

JACO_CONFIG="$WORK/jacod-1.yaml" "$WORK/jacod" >"$WORK/jacod-1.log" 2>&1 &
JACOD1_PID=$!
JACO_CONFIG="$WORK/jacod-2.yaml" "$WORK/jacod" >"$WORK/jacod-2.log" 2>&1 &
JACOD2_PID=$!
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco-1.sock" --name fanout 2>&1 | awk '/operator_token:/ {print $2}')
sleep 1
JOIN_TOK=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:27400 2>&1 | awk '/^Join token:/ {print $3}')
"$WORK/jaco" node join --socket "$WORK/jaco-2.sock" --peer 127.0.0.1:27400 --token "$JOIN_TOK"
sleep 2

cat > "$WORK/jaco.yaml" <<'EOF'
deployment: fanout
services:
  - name: web
    compose_service: web
    replicas: 2
EOF
cat > "$WORK/compose.yml" <<'EOF'
services:
  web:
    image: nginx:1.27
EOF
JACO_TOKEN="$TOKEN" "$WORK/jaco" apply "$WORK/jaco.yaml" \
  --server 127.0.0.1:27400 --compose "$WORK/compose.yml"
sleep 5

JACO_TOKEN="$TOKEN" "$WORK/jaco" logs fanout/web --server 127.0.0.1:27400 --follow >"$WORK/logs.out" 2>&1 &
LOGS_PID=$!
sleep 8
kill $LOGS_PID 2>/dev/null || true
wait $LOGS_PID 2>/dev/null || true

# Each LogLine carries host=<which-node>. Assert at least 2 distinct
# hosts appear in the captured stream.
HOSTS=$(awk '{for(i=1;i<=NF;i++) if($i ~ /^host=/) print $i}' "$WORK/logs.out" | sort -u | wc -l)
if [[ "$HOSTS" -lt 2 ]]; then
  echo "FAIL: log stream covered $HOSTS distinct hosts, want 2"
  head -20 "$WORK/logs.out"
  exit 1
fi
echo "PASS: logs-fanout"
