#!/usr/bin/env bash
# bootstrap.sh — stand up a 3-node JACO cluster on the testbed and deploy the
# shared workload. Runs from the OPERATOR host (your laptop / CI box). The
# operator reaches nodes only over their PUBLIC IPs (SSH); the in-cluster
# registry and the raft/gRPC/WG mesh ride the nodes' PRIVATE VNet IPs.
#
# Node addressing (node-1 first) comes from either:
#   * env: BENCH_PUBLIC_IPS / BENCH_PRIVATE_IPS (space-separated, same order)
#   * or Azure: RESOURCE_GROUP + VM_NAME_PREFIX (resolved via `az`)
#
# Env:
#   SSH_USER   default azureuser
#   SSH_KEY    default ~/.ssh/jaco
#   DEB        path to a prebuilt jaco_*.deb (default: built via `make package`)
#   REGISTRY   registry host:port (default: <node-1 private IP>:5000)
#   VNET_CIDR  default 172.16.0.0/16

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAMPLES_DIR="$(cd "$HERE/../.." && pwd)"
REPO_ROOT="$(cd "$SAMPLES_DIR/../.." && pwd)"   # tests/samples -> repo root
JACO_DIR="$SAMPLES_DIR/jaco"

SSH_USER="${SSH_USER:-azureuser}"
# Default to the per-bed key minted by the testbed deploy script.
_bed_key="$REPO_ROOT/tests/testbed/.ssh/jaco"
SSH_KEY="${SSH_KEY:-$([ -f "$_bed_key" ] && echo "$_bed_key" || echo "$HOME/.ssh/jaco")}"
VNET_CIDR="${VNET_CIDR:-172.16.0.0/16}"
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o UserKnownHostsFile=/dev/null)

# --- resolve node addresses (node-1 first) ----------------------------------
read -r -a PUB <<<"${BENCH_PUBLIC_IPS:-}"
read -r -a PRIV <<<"${BENCH_PRIVATE_IPS:-}"
if [[ ${#PUB[@]} -eq 0 || ${#PRIV[@]} -eq 0 ]]; then
  : "${RESOURCE_GROUP:?set BENCH_PUBLIC_IPS+BENCH_PRIVATE_IPS, or RESOURCE_GROUP for az resolution}"
  PREFIX="${VM_NAME_PREFIX:-jaco}"
  echo "[bootstrap] resolving node IPs from Azure RG=$RESOURCE_GROUP prefix=$PREFIX"
  mapfile -t PUB < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.publicIpAddresses[0].ipAddress')
  mapfile -t PRIV < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.privateIpAddresses[0]')
fi
[[ ${#PUB[@]} -ge 1 && ${#PUB[@]} -eq ${#PRIV[@]} ]] \
  || { echo "[bootstrap] could not resolve matching public/private IPs" >&2; exit 1; }

NODE1_PRIV="${PRIV[0]}"
REGISTRY="${REGISTRY:-${NODE1_PRIV}:5000}"
echo "[bootstrap] nodes:"
for i in "${!PUB[@]}"; do echo "  node$((i+1))  pub=${PUB[$i]}  priv=${PRIV[$i]}"; done
echo "[bootstrap] registry: $REGISTRY"

ssh_node() { ssh "${SSH_OPTS[@]}" "$SSH_USER@$1" "$2"; }

# --- 1. build the .deb (unless provided) ------------------------------------
if [[ -z "${DEB:-}" ]]; then
  echo "[bootstrap] building jaco .deb (make package — needs nfpm on PATH)"
  ( cd "$REPO_ROOT" && PATH="$HOME/go/bin:$PATH" make package )
  # nfpm writes to dist/package/<arch>/, so search recursively (not dist/*.deb).
  DEB="$(find "$REPO_ROOT/dist" -name '*amd64.deb' -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2)"
fi
[[ -f "$DEB" ]] || { echo "[bootstrap] no .deb found ($DEB); set DEB=path" >&2; exit 1; }
echo "[bootstrap] using package: $DEB"

# --- 2. install docker + jacod on every node --------------------------------
for i in "${!PUB[@]}"; do
  pub="${PUB[$i]}"
  echo "[bootstrap] installing node$((i+1)) ($pub)"
  scp "${SSH_OPTS[@]}" "$DEB" "$HERE/install-node.sh" "$SSH_USER@$pub:/tmp/"
  ssh_node "$pub" "sudo bash /tmp/install-node.sh /tmp/$(basename "$DEB") '$REGISTRY' '$VNET_CIDR'"
done

# --- 3. in-cluster registry + image build/push (on node-1) ------------------
echo "[bootstrap] starting registry on node-1"
# docker runs as root on the nodes (azureuser isn't in the docker group; only
# the jaco service user is), so every node-side docker call goes through sudo.
ssh_node "${PUB[0]}" "sudo docker inspect registry >/dev/null 2>&1 || sudo docker run -d --restart=always --name registry -p 5000:5000 registry:2"

# Stack-scoped env file lives next to jaco.yaml and feeds ${VAR}
# interpolation across docker-compose.yml on the CLI side (the daemon never
# reads operator-side files). REGISTRY is pinned to node-1's PRIVATE VNet IP
# because Azure VNet doesn't resolve bare VM names across VMs — non-builder
# nodes need a routable address to pull the custom images. Created here at
# bootstrap time; gitignored so secrets never land in commits.
PG_USER="${PG_USER:-bench}"
PG_PASSWORD="${PG_PASSWORD:-bench}"
PG_DB="${PG_DB:-bench}"

echo "[bootstrap] shipping workload build contexts + manifest pair to node-1"
ssh_node "${PUB[0]}" "rm -rf ~/bench && mkdir -p ~/bench"
scp -r "${SSH_OPTS[@]}" "$SAMPLES_DIR/workload" "$SSH_USER@${PUB[0]}:~/bench/workload"
scp -r "${SSH_OPTS[@]}" "$JACO_DIR/jaco.yaml" "$JACO_DIR/docker-compose.yml" "$SSH_USER@${PUB[0]}:~/bench/"

# Write .env on the node itself: keeps creds off the operator's disk and out
# of any local scp log. jaco apply runs under sudo but reads .env from the
# CWD relative to jaco.yaml, so file mode 0640 is fine (azureuser owns it,
# root reads it).
ssh_node "${PUB[0]}" "cat > ~/bench/.env <<EOF
REGISTRY=$REGISTRY
PG_USER=$PG_USER
PG_PASSWORD=$PG_PASSWORD
PG_DB=$PG_DB
EOF
chmod 0640 ~/bench/.env"

echo "[bootstrap] building + pushing workload images on node-1 -> $REGISTRY"
ssh_node "${PUB[0]}" "
  set -e
  sudo docker build -t '$REGISTRY/bench-web:latest'      ~/bench/workload/web
  sudo docker build -t '$REGISTRY/bench-api:latest'      ~/bench/workload/api
  sudo docker build -t '$REGISTRY/bench-postgres:latest' ~/bench/workload/postgres
  sudo docker push '$REGISTRY/bench-web:latest'
  sudo docker push '$REGISTRY/bench-api:latest'
  sudo docker push '$REGISTRY/bench-postgres:latest'
"

# --- 4. form the cluster -----------------------------------------------------
echo "[bootstrap] initializing cluster on node-1"
ssh_node "${PUB[0]}" "sudo jaco cluster init || true"
# Persist jacod across reboots — postinstall ships disabled by design (build/packaging/postinstall.sh); enabling here is the cluster-commit signal. Issue #151.
ssh_node "${PUB[0]}" "sudo systemctl enable jaco"

# Join tokens are single-use — issue a fresh one for each joining node.
for i in "${!PUB[@]}"; do
  [[ "$i" -eq 0 ]] && continue
  echo "[bootstrap] issuing join token for node$((i+1))"
  TOKEN="$(ssh_node "${PUB[0]}" "sudo jaco node issue-join-token" | grep -oE 'token=[^ ]+' | head -1 | cut -d= -f2)"
  [[ -n "$TOKEN" ]] || { echo "[bootstrap] failed to capture join token" >&2; exit 1; }
  echo "[bootstrap] joining node$((i+1)) -> ${NODE1_PRIV}:7000"
  ssh_node "${PUB[$i]}" "sudo jaco node join --peer='${NODE1_PRIV}:7000' --token='$TOKEN'"
  # Persist jacod across reboots — postinstall ships disabled by design (build/packaging/postinstall.sh); enabling here is the cluster-commit signal. Issue #151.
  ssh_node "${PUB[$i]}" "sudo systemctl enable jaco"
done

echo "[bootstrap] cluster status:"
ssh_node "${PUB[0]}" "sudo jaco cluster status" || true

# --- 5. deploy the workload --------------------------------------------------
# REGISTRY/PG_USER/etc. now come from the .env file the CLI loads via the
# jaco.yaml top-level `environment:` field — no process-env passthrough
# needed on the apply line.
echo "[bootstrap] applying the workload (interpolating from ~/bench/.env)"
ssh_node "${PUB[0]}" "cd ~/bench && sudo jaco apply jaco.yaml --compose docker-compose.yml"

echo
echo "[bootstrap] done. Ingress is served at the LB public IP (jaco.sh) on 80/443."
echo "[bootstrap] check rollout:  ssh $SSH_USER@${PUB[0]} 'sudo jaco status'"
