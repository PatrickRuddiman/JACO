#!/usr/bin/env bash
# apply-deploy.sh — E2E: boot jacod, cluster init, jaco apply, assert
# the deployment is ACTIVE in `jaco status`, then delete + assert it's
# gone.
#
# Gated by JACO_APPLY_DEPLOY_FORCE=1. The default skip lets CI pass on
# unprivileged runners that can't run jacod or reach docker.

set -euo pipefail

if [[ "${JACO_APPLY_DEPLOY_FORCE:-0}" != "1" ]]; then
  cat <<'EOF'
SKIP apply-deploy.sh: set JACO_APPLY_DEPLOY_FORCE=1 to enable.

This E2E boots jacod, runs `jaco cluster init`, `jaco apply` against a
hello-world manifest, asserts `jaco status` shows the deployment ACTIVE,
then deletes it and asserts the replicas tear down.
EOF
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-apply-deploy-XXXX)"
trap 'kill $JACOD_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

go build -o "$WORK/jacod" ./cmd/jacod
go build -o "$WORK/jaco"  ./cmd/jaco

mkdir -p "$WORK/data"
cat > "$WORK/jacod.yaml" <<EOF
data_dir: $WORK/data
listen_addr: 127.0.0.1:27000
cluster_addr: 127.0.0.1:27001
unix_socket: $WORK/jaco.sock
wg_port: 51820
log_level: info
ipam_pool: 10.244.0.0/16
EOF

cat > "$WORK/jaco.yaml" <<'EOF'
deployment: smoke
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

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco.sock" --name smoke 2>&1 | awk '/operator_token:/ {print $2}')
[[ -z "$TOKEN" ]] && { echo "FAIL: empty operator token"; exit 1; }
sleep 1

JACO_TOKEN="$TOKEN" "$WORK/jaco" apply "$WORK/jaco.yaml" \
  --server 127.0.0.1:27000 --compose "$WORK/compose.yml" \
  || { echo "FAIL: apply"; exit 1; }

STATUS=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" status --server 127.0.0.1:27000 smoke 2>&1)
echo "$STATUS" | grep -qE "smoke.*ACTIVE" || { echo "FAIL: status not ACTIVE"; echo "$STATUS"; exit 1; }

JACO_TOKEN="$TOKEN" "$WORK/jaco" delete smoke --server 127.0.0.1:27000 \
  || { echo "FAIL: delete"; exit 1; }
sleep 1

STATUS=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" status --server 127.0.0.1:27000 smoke 2>&1 || true)
if echo "$STATUS" | grep -qE "smoke.*ACTIVE"; then
  echo "FAIL: deployment still ACTIVE after delete"
  echo "$STATUS"
  exit 1
fi

echo "PASS: apply-deploy"
