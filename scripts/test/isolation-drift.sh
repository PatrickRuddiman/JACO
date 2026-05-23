#!/usr/bin/env bash
# isolation-drift.sh — E2E: boot jacod with nftables available, apply a
# deployment that creates a subnet, drift the live ruleset by deleting
# the JACO table, wait for the firewall.Reconciler tick, assert the
# table is recreated.
#
# Gated by JACO_ISOLATION_DRIFT_FORCE=1. Needs nft + CAP_NET_ADMIN.

set -euo pipefail

if [[ "${JACO_ISOLATION_DRIFT_FORCE:-0}" != "1" ]]; then
  echo "SKIP isolation-drift.sh: set JACO_ISOLATION_DRIFT_FORCE=1 to enable."
  exit 0
fi
if ! command -v nft >/dev/null 2>&1; then
  echo "SKIP isolation-drift.sh: nft binary not found on PATH"
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-isolation-drift-XXXX)"
trap 'kill $JACOD_PID 2>/dev/null || true; nft delete table inet jaco 2>/dev/null || true; rm -rf "$WORK"' EXIT

go build -o "$WORK/jacod" ./cmd/jacod
go build -o "$WORK/jaco"  ./cmd/jaco

mkdir -p "$WORK/data"
cat > "$WORK/jacod.yaml" <<EOF
data_dir: $WORK/data
listen_addr: 127.0.0.1:27600
cluster_addr: 127.0.0.1:27601
unix_socket: $WORK/jaco.sock
wg_port: 51820
log_level: info
ipam_pool: 10.244.0.0/16
EOF

JACO_CONFIG="$WORK/jacod.yaml" "$WORK/jacod" >"$WORK/jacod.log" 2>&1 &
JACOD_PID=$!
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco.sock" --name drift 2>&1 | awk '/operator_token:/ {print $2}')

# First reconciler tick fires within ~1s of Init → table should exist.
sleep 2
nft list table inet jaco >/dev/null 2>&1 || { echo "FAIL: table not created initially"; exit 1; }

# Drift the live state by tearing the table down.
nft delete table inet jaco

# Reconciler runs on a 30s ticker; wait up to 45s for re-apply.
deadline=$((SECONDS + 45))
while (( SECONDS < deadline )); do
  if nft list table inet jaco >/dev/null 2>&1; then
    echo "PASS: isolation-drift (table recreated after $((SECONDS)) s)"
    exit 0
  fi
  sleep 2
done
echo "FAIL: table never recreated within 45s"
exit 1
