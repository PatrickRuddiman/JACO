#!/usr/bin/env bash
# cluster-join.sh — E2E: boot 2 jacods, cluster init on node-1, mint
# join token, jaco node join on node-2, assert `jaco node list` shows
# both nodes in NODE_STATUS_READY.
#
# Gated by JACO_CLUSTER_JOIN_FORCE=1.

set -euo pipefail

if [[ "${JACO_CLUSTER_JOIN_FORCE:-0}" != "1" ]]; then
  echo "SKIP cluster-join.sh: set JACO_CLUSTER_JOIN_FORCE=1 to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-cluster-join-XXXX)"
trap 'kill $J1 $J2 2>/dev/null || true; rm -rf "$WORK"' EXIT

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
mkconfig 1 28100 28110
mkconfig 2 28200 28210

JACO_CONFIG="$WORK/jacod-1.yaml" "$WORK/jacod" >"$WORK/jacod-1.log" 2>&1 &
J1=$!
JACO_CONFIG="$WORK/jacod-2.yaml" "$WORK/jacod" >"$WORK/jacod-2.log" 2>&1 &
J2=$!
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco-1.sock" --name join 2>&1 | awk '/operator_token:/ {print $2}')
sleep 1
JOIN_TOK=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:28100 2>&1 | awk '/^Join token:/ {print $3}')
"$WORK/jaco" node join --socket "$WORK/jaco-2.sock" --peer 127.0.0.1:28100 --token "$JOIN_TOK" \
  || { echo "FAIL: node join"; exit 1; }
sleep 2

LIST=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node list --server 127.0.0.1:28100 2>&1)
READY_COUNT=$(echo "$LIST" | grep -cE "NODE_STATUS_READY" || true)
if [[ "$READY_COUNT" -lt 2 ]]; then
  echo "FAIL: only $READY_COUNT nodes in READY"
  echo "$LIST"
  exit 1
fi
echo "PASS: cluster-join ($READY_COUNT nodes READY)"
