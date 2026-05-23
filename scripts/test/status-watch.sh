#!/usr/bin/env bash
# status-watch.sh — E2E: boot jacod, run `jaco status -w` in the
# background, jaco apply a deployment, assert the watch stream emitted
# at least 2 snapshots (initial empty + post-apply).
#
# Gated by JACO_STATUS_WATCH_FORCE=1.

set -euo pipefail

if [[ "${JACO_STATUS_WATCH_FORCE:-0}" != "1" ]]; then
  echo "SKIP status-watch.sh: set JACO_STATUS_WATCH_FORCE=1 to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-status-watch-XXXX)"
trap 'kill $JACOD_PID 2>/dev/null || true; kill $WATCH_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

go build -o "$WORK/jacod" ./cmd/jacod
go build -o "$WORK/jaco"  ./cmd/jaco

mkdir -p "$WORK/data"
cat > "$WORK/jacod.yaml" <<EOF
data_dir: $WORK/data
listen_addr: 127.0.0.1:27100
cluster_addr: 127.0.0.1:27101
unix_socket: $WORK/jaco.sock
wg_port: 51820
log_level: info
ipam_pool: 10.244.0.0/16
EOF

cat > "$WORK/jaco.yaml" <<'EOF'
deployment: watched
services:
  - name: web
    compose_service: web
    replicas: 1
EOF
cat > "$WORK/compose.yml" <<'EOF'
services:
  web:
    image: nginx:1.27
EOF

JACO_CONFIG="$WORK/jacod.yaml" "$WORK/jacod" >"$WORK/jacod.log" 2>&1 &
JACOD_PID=$!
sleep 2
TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco.sock" --name watch 2>&1 | awk '/operator_token:/ {print $2}')
[[ -z "$TOKEN" ]] && { echo "FAIL: empty operator token"; exit 1; }
sleep 1

# Start watcher.
JACO_TOKEN="$TOKEN" "$WORK/jaco" status --server 127.0.0.1:27100 -w >"$WORK/watch.log" 2>&1 &
WATCH_PID=$!
sleep 1

JACO_TOKEN="$TOKEN" "$WORK/jaco" apply "$WORK/jaco.yaml" \
  --server 127.0.0.1:27100 --compose "$WORK/compose.yml" \
  || { echo "FAIL: apply"; exit 1; }
sleep 2

kill $WATCH_PID 2>/dev/null || true
wait $WATCH_PID 2>/dev/null || true

# Watch output should contain at least 2 "Deployments:" headers (initial
# empty + post-apply snapshot).
COUNT=$(grep -c "^Deployments:" "$WORK/watch.log" || true)
if [[ "$COUNT" -lt 2 ]]; then
  echo "FAIL: watch emitted $COUNT snapshots, want >=2"
  cat "$WORK/watch.log"
  exit 1
fi
echo "PASS: status-watch ($COUNT snapshots)"
