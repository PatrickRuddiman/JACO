#!/usr/bin/env bash
# bootstrap.sh — stand up a 3-node Docker Swarm on the testbed and deploy the
# shared bench workload. Runs from the OPERATOR host. Mirrors the JACO bootstrap
# flow (install → registry → build/push → form cluster → deploy) so the two are
# directly comparable; the differences are Swarm init/join + `docker stack
# deploy` instead of jacod + `jaco apply`.
#
# Node addressing (node-1 first) comes from either:
#   * env: BENCH_PUBLIC_IPS / BENCH_PRIVATE_IPS (space-separated, same order)
#   * or Azure: RESOURCE_GROUP + VM_NAME_PREFIX (resolved via `az`)
#
# Env: SSH_USER (azureuser), SSH_KEY (default the bed key), REGISTRY
# (default <node-1 private IP>:5000), VNET_CIDR (172.16.0.0/16).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAMPLES_DIR="$(cd "$HERE/../.." && pwd)"
REPO_ROOT="$(cd "$SAMPLES_DIR/../.." && pwd)"
SWARM_DIR="$SAMPLES_DIR/swarm"

SSH_USER="${SSH_USER:-azureuser}"
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
  echo "[swarm] resolving node IPs from Azure RG=$RESOURCE_GROUP prefix=$PREFIX"
  mapfile -t PUB < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.publicIpAddresses[0].ipAddress')
  mapfile -t PRIV < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.privateIpAddresses[0]')
fi
[[ ${#PUB[@]} -ge 1 && ${#PUB[@]} -eq ${#PRIV[@]} ]] \
  || { echo "[swarm] could not resolve matching public/private IPs" >&2; exit 1; }

NODE1_PRIV="${PRIV[0]}"
REGISTRY="${REGISTRY:-${NODE1_PRIV}:5000}"
echo "[swarm] nodes:"
for i in "${!PUB[@]}"; do echo "  node$((i+1))  pub=${PUB[$i]}  priv=${PRIV[$i]}"; done
echo "[swarm] registry: $REGISTRY"

ssh_node() { ssh "${SSH_OPTS[@]}" "$SSH_USER@$1" "$2"; }

# --- 1. install docker on every node ----------------------------------------
for i in "${!PUB[@]}"; do
  pub="${PUB[$i]}"
  echo "[swarm] installing docker on node$((i+1)) ($pub)"
  scp "${SSH_OPTS[@]}" "$HERE/install-node.sh" "$SSH_USER@$pub:/tmp/"
  ssh_node "$pub" "sudo bash /tmp/install-node.sh '$REGISTRY' '$VNET_CIDR'"
done

# --- 2. in-cluster registry + image build/push (on node-1) ------------------
echo "[swarm] starting registry on node-1"
ssh_node "${PUB[0]}" "sudo docker inspect registry >/dev/null 2>&1 || sudo docker run -d --restart=always --name registry -p 5000:5000 registry:2"

echo "[swarm] shipping workload build contexts to node-1"
ssh_node "${PUB[0]}" "rm -rf ~/swarm-bench && mkdir -p ~/swarm-bench"
scp -r "${SSH_OPTS[@]}" "$SAMPLES_DIR/workload" "$SSH_USER@${PUB[0]}:~/swarm-bench/workload"
scp "${SSH_OPTS[@]}" "$SWARM_DIR/stack.yml" "$SSH_USER@${PUB[0]}:~/swarm-bench/"
# Caddyfile rides next to stack.yml — `configs.file: ./Caddyfile` resolves
# relative to the deploy CWD (~/swarm-bench), so `docker stack deploy` creates
# the Swarm config from it.
scp "${SSH_OPTS[@]}" "$SWARM_DIR/Caddyfile" "$SSH_USER@${PUB[0]}:~/swarm-bench/"

echo "[swarm] building + pushing workload images on node-1 -> $REGISTRY"
ssh_node "${PUB[0]}" "
  set -e
  sudo docker build -t '$REGISTRY/bench-web:latest'      ~/swarm-bench/workload/web
  sudo docker build -t '$REGISTRY/bench-api:latest'      ~/swarm-bench/workload/api
  sudo docker build -t '$REGISTRY/bench-postgres:latest' ~/swarm-bench/workload/postgres
  sudo docker push '$REGISTRY/bench-web:latest'
  sudo docker push '$REGISTRY/bench-api:latest'
  sudo docker push '$REGISTRY/bench-postgres:latest'
"

# --- 3. form the swarm -------------------------------------------------------
echo "[swarm] init on node-1 (advertise ${NODE1_PRIV})"
ssh_node "${PUB[0]}" "sudo docker swarm init --advertise-addr '${NODE1_PRIV}' >/dev/null 2>&1 || true"
TOKEN="$(ssh_node "${PUB[0]}" "sudo docker swarm join-token -q worker")"
[[ -n "$TOKEN" ]] || { echo "[swarm] failed to get worker join token" >&2; exit 1; }
for i in "${!PUB[@]}"; do
  [[ "$i" -eq 0 ]] && continue
  echo "[swarm] joining node$((i+1)) as worker -> ${NODE1_PRIV}:2377"
  ssh_node "${PUB[$i]}" "sudo docker swarm join --token '$TOKEN' '${NODE1_PRIV}:2377' >/dev/null 2>&1 || true"
done
echo "[swarm] nodes:"
ssh_node "${PUB[0]}" "sudo docker node ls" || true

# --- 4. deploy the stack -----------------------------------------------------
# Pin the registry to node-1's private IP in the stack file: the default is the
# hostname jaco-1:5000, which Azure VNet doesn't resolve across VMs, so workers
# couldn't pull the custom images. The private IP always resolves and is in each
# node's insecure-registries. (Same fix as the JACO bench.)
echo "[swarm] deploying stack 'bench' (REGISTRY=$REGISTRY)"
ssh_node "${PUB[0]}" "sed -i 's|[\$]{REGISTRY:-jaco-1:5000}|$REGISTRY|g' ~/swarm-bench/stack.yml"
ssh_node "${PUB[0]}" "cd ~/swarm-bench && sudo docker stack deploy --detach=true -c stack.yml bench"

echo
echo "[swarm] done. Caddy terminates TLS for jaco.sh (ACME staging) and proxies"
echo "[swarm]   to web; :80/:443 are published on all nodes via the routing mesh"
echo "[swarm]   (behind the LB). Bench over HTTPS, same as JACO:"
echo "[swarm]   BENCH_TARGET=https://jaco.sh  (k6 uses insecureSkipTLSVerify)"
echo "[swarm] check rollout:  ssh $SSH_USER@${PUB[0]} 'sudo docker stack services bench'"
