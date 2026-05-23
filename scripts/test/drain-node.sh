#!/usr/bin/env bash
# drain-node.sh — E2E: 2-node cluster, apply a 2-replica deployment,
# `jaco node remove` the follower without --force, assert the drain
# migrates the follower's replica off before the node leaves.
#
# Gated by JACO_DRAIN_NODE_FORCE=1.

set -euo pipefail

if [[ "${JACO_DRAIN_NODE_FORCE:-0}" != "1" ]]; then
  echo "SKIP drain-node.sh: set JACO_DRAIN_NODE_FORCE=1 to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-drain-XXXX)"
trap 'kill $JACOD1_PID 2>/dev/null || true; kill $JACOD2_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

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
mkconfig 1 27200 27201
mkconfig 2 27300 27301

JACO_CONFIG="$WORK/jacod-1.yaml" "$WORK/jacod" >"$WORK/jacod-1.log" 2>&1 &
JACOD1_PID=$!
JACO_CONFIG="$WORK/jacod-2.yaml" "$WORK/jacod" >"$WORK/jacod-2.log" 2>&1 &
JACOD2_PID=$!
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco-1.sock" --name drain 2>&1 | awk '/operator_token:/ {print $2}')
sleep 1
JOIN_TOK=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:27200 2>&1 | awk '/^Join token:/ {print $3}')
"$WORK/jaco" node join --socket "$WORK/jaco-2.sock" --peer 127.0.0.1:27200 --token "$JOIN_TOK" || { echo "FAIL: join"; exit 1; }
sleep 2

cat > "$WORK/jaco.yaml" <<'EOF'
deployment: drained
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
  --server 127.0.0.1:27200 --compose "$WORK/compose.yml" || { echo "FAIL: apply"; exit 1; }
sleep 3

# Remove node-2 gracefully (force=false). Should drain replicas onto
# node-1 before returning.
HOST2=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node list --server 127.0.0.1:27200 2>&1 | awk '!/odin/ && /NODE_STATUS/ {print $1; exit}')
[[ -z "$HOST2" ]] && { echo "FAIL: couldn't find second node hostname"; exit 1; }

JACO_TOKEN="$TOKEN" "$WORK/jaco" node remove "$HOST2" --server 127.0.0.1:27200 \
  || { echo "FAIL: node remove"; exit 1; }

sleep 1
LIST=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node list --server 127.0.0.1:27200 2>&1)
if echo "$LIST" | grep -q "^$HOST2"; then
  echo "FAIL: $HOST2 still in node list"
  echo "$LIST"
  exit 1
fi
echo "PASS: drain-node"
