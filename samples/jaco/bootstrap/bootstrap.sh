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
REPO_ROOT="$(cd "$SAMPLES_DIR/.." && pwd)"
JACO_DIR="$SAMPLES_DIR/jaco"

SSH_USER="${SSH_USER:-azureuser}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/jaco}"
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
  echo "[bootstrap] building jaco .deb (make package — needs nfpm)"
  ( cd "$REPO_ROOT" && make package )
  DEB="$(ls -t "$REPO_ROOT"/dist/*amd64.deb 2>/dev/null | head -1)"
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
ssh_node "${PUB[0]}" "docker inspect registry >/dev/null 2>&1 || docker run -d --restart=always --name registry -p 5000:5000 registry:2"

echo "[bootstrap] shipping workload build contexts to node-1"
ssh_node "${PUB[0]}" "rm -rf ~/bench && mkdir -p ~/bench"
scp -r "${SSH_OPTS[@]}" "$SAMPLES_DIR/workload" "$SSH_USER@${PUB[0]}:~/bench/workload"
scp -r "${SSH_OPTS[@]}" "$JACO_DIR/jaco.yaml" "$JACO_DIR/docker-compose.yml" "$SSH_USER@${PUB[0]}:~/bench/"

echo "[bootstrap] building + pushing workload images on node-1 -> $REGISTRY"
ssh_node "${PUB[0]}" "
  set -e
  docker build -t '$REGISTRY/bench-web:latest' ~/bench/workload/web
  docker build -t '$REGISTRY/bench-api:latest' ~/bench/workload/api
  docker push '$REGISTRY/bench-web:latest'
  docker push '$REGISTRY/bench-api:latest'
"

# --- 4. form the cluster -----------------------------------------------------
echo "[bootstrap] initializing cluster on node-1"
ssh_node "${PUB[0]}" "sudo jaco cluster init || true"

echo "[bootstrap] issuing join token on node-1"
TOKEN="$(ssh_node "${PUB[0]}" "sudo jaco node issue-join-token" | grep -oE 'token=[^ ]+' | head -1 | cut -d= -f2)"
[[ -n "$TOKEN" ]] || { echo "[bootstrap] failed to capture join token" >&2; exit 1; }

for i in "${!PUB[@]}"; do
  [[ "$i" -eq 0 ]] && continue
  echo "[bootstrap] joining node$((i+1)) -> ${NODE1_PRIV}:7000"
  ssh_node "${PUB[$i]}" "sudo jaco node join --peer='${NODE1_PRIV}:7000' --token='$TOKEN'"
done

echo "[bootstrap] cluster status:"
ssh_node "${PUB[0]}" "sudo jaco cluster status" || true

# --- 5. deploy the workload --------------------------------------------------
echo "[bootstrap] applying the workload (REGISTRY=$REGISTRY)"
ssh_node "${PUB[0]}" "cd ~/bench && sudo REGISTRY='$REGISTRY' jaco apply --jaco jaco.yaml --compose docker-compose.yml"

echo
echo "[bootstrap] done. Ingress is served at the LB public IP (jaco.sh) on 80/443."
echo "[bootstrap] check rollout:  ssh $SSH_USER@${PUB[0]} 'sudo jaco status'"
