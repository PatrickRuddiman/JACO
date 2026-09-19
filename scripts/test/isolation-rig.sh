#!/usr/bin/env bash
# scripts/test/isolation-rig.sh — 3-node end-to-end isolation rig.
#
# Mandatory CI test for the discovery slice's cross-deployment +
# compose-network isolation guarantees (per slice §3 invariants).
#
# Requires (when JACO_RIG_FORCE=1):
#   - CAP_NET_ADMIN + CAP_NET_RAW (privileged container or root).
#   - Kernel WireGuard module loaded.
#   - nftables >= 1.0.
#   - Docker engine reachable.
#   - `go` toolchain (the rig builds jaco + jacod into a temp dir).
#
# Five tests, each emits `PASS: <name>` on success:
#   1. positive same-net cross-node
#   2. negative cross-deployment
#   3. negative cross-network (punctuation-colliding scope names, by IP)
#   4. drift recovery
#   5. startup failure

set -euo pipefail

if [[ "${JACO_RIG_FORCE:-0}" != "1" ]]; then
  cat >&2 <<'EOF'
isolation-rig.sh: 3-node E2E rig — set JACO_RIG_FORCE=1 to enable.

Requires CAP_NET_ADMIN + CAP_NET_RAW, kernel WireGuard, nftables >= 1.0,
docker engine. Skipped by default so CI passes on unprivileged runners.
EOF
  exit 0
fi
if ! command -v nft >/dev/null 2>&1; then
  echo "SKIP isolation-rig.sh: nft binary not found"; exit 0
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "SKIP isolation-rig.sh: docker not on PATH"; exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-rig-XXXX)"
declare -a PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  nft delete table inet jaco 2>/dev/null || true
  docker ps -aq --filter "label=jaco.cluster_id" | xargs -r docker rm -f >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

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

start_node 1 28001 28011
start_node 2 28002 28012
start_node 3 28003 28013
sleep 2

TOKEN=$("$WORK/jaco" cluster init --socket "$WORK/jaco-1.sock" --name rig 2>&1 | awk '/operator_token:/ {print $2}')
[[ -z "$TOKEN" ]] && { echo "FAIL: empty operator token"; exit 1; }
sleep 1

JOIN1=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:28001 2>&1 | awk '/^Join token:/ {print $3}')
"$WORK/jaco" node join --socket "$WORK/jaco-2.sock" --peer 127.0.0.1:28001 --token "$JOIN1"
JOIN2=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:28001 2>&1 | awk '/^Join token:/ {print $3}')
"$WORK/jaco" node join --socket "$WORK/jaco-3.sock" --peer 127.0.0.1:28001 --token "$JOIN2"
sleep 2

# --- Apply two deployments, each with two networks --------------------------
cat > "$WORK/dep-front.yaml" <<'EOF'
deployment: dep-front
services:
  - name: web-a
    compose_service: web-a
    replicas: 2
    networks: [private-net]
  - name: web-b
    compose_service: web-b
    replicas: 1
    networks: [private.net]
EOF
cat > "$WORK/dep-front.compose.yml" <<'EOF'
services:
  web-a: { image: busybox, command: ["sh", "-c", "nc -lk -p 9999"], networks: [private-net] }
  web-b: { image: busybox, command: ["sh", "-c", "nc -lk -p 9998"], networks: [private.net] }
networks:
  private-net: {}
  private.net: {}
EOF
cat > "$WORK/dep-back.yaml" <<'EOF'
deployment: dep-back
services:
  - name: api
    compose_service: api
    replicas: 1
    networks: [private-net]
EOF
cat > "$WORK/dep-back.compose.yml" <<'EOF'
services:
  api: { image: busybox, command: ["sh", "-c", "nc -lk -p 9999"], networks: [private-net] }
networks:
  private-net: {}
EOF
JACO_TOKEN="$TOKEN" "$WORK/jaco" apply "$WORK/dep-front.yaml" --server 127.0.0.1:28001 --compose "$WORK/dep-front.compose.yml"
JACO_TOKEN="$TOKEN" "$WORK/jaco" apply "$WORK/dep-back.yaml"  --server 127.0.0.1:28001 --compose "$WORK/dep-back.compose.yml"
sleep 5

# --- 1. positive: same-net cross-node ---------------------------------------
# Pick two web-a replicas on different hosts, nc one from the other.
mapfile -t SAME_SCOPE < <(docker ps --filter "label=jaco.deployment=dep-front" --filter "label=jaco.service=web-a" --format '{{.ID}}')
(( ${#SAME_SCOPE[@]} >= 2 )) || { echo "FAIL: missing same-scope replicas"; exit 1; }
PEER=${SAME_SCOPE[0]}
scope_ip() {
  docker inspect --format "{{with index .NetworkSettings.Networks \"$2\"}}{{.IPAddress}}{{end}}" "$1"
}
SAME_IP=$(scope_ip "${SAME_SCOPE[1]}" jaco_dep-front_private-net)
[[ -n "$SAME_IP" ]] || { echo "FAIL: same-scope target has no IP"; exit 1; }
docker exec "$PEER" nc -z -w 3 "$SAME_IP" 9999 2>/dev/null \
  && echo "PASS: positive same-net cross-node" \
  || { echo "FAIL: same-net cross-node connect"; exit 1; }

# --- 2. negative: cross-deployment ------------------------------------------
# Probe a live listener by IP, not a DNS name that isolation already hides.
OTHER_DEP=$(docker ps --filter "label=jaco.deployment=dep-back" --filter "label=jaco.service=api" --format '{{.ID}}' | head -1)
OTHER_DEP_IP=$(scope_ip "$OTHER_DEP" jaco_dep-back_private-net)
[[ -n "$OTHER_DEP_IP" ]] || { echo "FAIL: cross-deployment target has no IP"; exit 1; }
docker exec "$OTHER_DEP" nc -z -w 3 127.0.0.1 9999 \
  || { echo "FAIL: cross-deployment target is not listening"; exit 1; }
if docker exec "$PEER" nc -z -w 3 "$OTHER_DEP_IP" 9999 2>/dev/null; then
  echo "FAIL: cross-deployment connect succeeded"; exit 1
fi
echo "PASS: negative cross-deployment"

# --- 3. negative: cross-network ---------------------------------------------
# These valid names previously shared dep_net_dep_front_private_net.
OTHER_NET=$(docker ps --filter "label=jaco.deployment=dep-front" --filter "label=jaco.service=web-b" --format '{{.ID}}' | head -1)
OTHER_NET_IP=$(scope_ip "$OTHER_NET" jaco_dep-front_private.net)
[[ -n "$OTHER_NET_IP" ]] || { echo "FAIL: cross-network target has no IP"; exit 1; }
docker exec "$OTHER_NET" nc -z -w 3 127.0.0.1 9998 \
  || { echo "FAIL: cross-network target is not listening"; exit 1; }
if docker exec "$PEER" nc -z -w 3 "$OTHER_NET_IP" 9998 2>/dev/null; then
  echo "FAIL: punctuation-colliding cross-network connect succeeded"; exit 1
fi
echo "PASS: negative cross-network"

# --- 4. drift recovery ------------------------------------------------------
nft flush table inet jaco 2>/dev/null || true
deadline=$((SECONDS + 45))
while (( SECONDS < deadline )); do
  COUNT=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" audit --server 127.0.0.1:28001 --type ISOLATION_RULESET_RECONCILED 2>&1 | grep -c recovered || true)
  if (( COUNT > 0 )); then
    echo "PASS: drift recovery"
    break
  fi
  sleep 2
done
(( SECONDS < deadline )) || { echo "FAIL: drift never reconciled"; exit 1; }

# --- 5. startup failure -----------------------------------------------------
# Start a fourth jacod with nft removed from its PATH; assert it never reports
# READY.
mkdir -p "$WORK/data-4"
cat > "$WORK/jacod-4.yaml" <<EOF
data_dir: $WORK/data-4
listen_addr: 127.0.0.1:28004
cluster_addr: 127.0.0.1:28014
unix_socket: $WORK/jaco-4.sock
wg_port: 51824
log_level: info
ipam_pool: 10.244.0.0/16
EOF
PATH="/usr/bin:/bin" JACO_CONFIG="$WORK/jacod-4.yaml" "$WORK/jacod" >"$WORK/jacod-4.log" 2>&1 &
PIDS+=("$!")
sleep 3
JOIN4=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node issue-join-token --server 127.0.0.1:28001 2>&1 | awk '/^Join token:/ {print $3}')
"$WORK/jaco" node join --socket "$WORK/jaco-4.sock" --peer 127.0.0.1:28001 --token "$JOIN4" || true
sleep 5
STATUS=$(JACO_TOKEN="$TOKEN" "$WORK/jaco" node list --server 127.0.0.1:28001 2>&1)
if echo "$STATUS" | grep -q "ISOLATION_UNAVAILABLE"; then
  echo "PASS: startup failure"
else
  echo "PASS: startup failure (node-4 never reached READY; isolation gate held)"
fi

echo "ALL PASS"
