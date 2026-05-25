#!/usr/bin/env bash
# bootstrap.sh — stand up a 3-node k3s cluster on the testbed and deploy the
# shared bench workload. Runs from the OPERATOR host. Mirrors the swarm/jaco
# bootstrap flow (install → registry → build/push → form cluster → deploy); the
# differences are k3s server/agent install + `kubectl apply` instead of `docker
# swarm` + `docker stack deploy`.
#
# Node addressing (node-1 first = the k3s server) comes from either:
#   * env: BENCH_PUBLIC_IPS / BENCH_PRIVATE_IPS (space-separated, same order)
#   * or Azure: RESOURCE_GROUP + VM_NAME_PREFIX (resolved via `az`)
#
# Env: SSH_USER (azureuser), SSH_KEY (default the bed key), REGISTRY
# (default <node-1 private IP>:5000).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAMPLES_DIR="$(cd "$HERE/../.." && pwd)"
REPO_ROOT="$(cd "$SAMPLES_DIR/../.." && pwd)"
K3S_DIR="$SAMPLES_DIR/k3s"

SSH_USER="${SSH_USER:-azureuser}"
_bed_key="$REPO_ROOT/tests/testbed/.ssh/jaco"
SSH_KEY="${SSH_KEY:-$([ -f "$_bed_key" ] && echo "$_bed_key" || echo "$HOME/.ssh/jaco")}"
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o UserKnownHostsFile=/dev/null)

# --- resolve node addresses (node-1 first) ----------------------------------
read -r -a PUB <<<"${BENCH_PUBLIC_IPS:-}"
read -r -a PRIV <<<"${BENCH_PRIVATE_IPS:-}"
if [[ ${#PUB[@]} -eq 0 || ${#PRIV[@]} -eq 0 ]]; then
  : "${RESOURCE_GROUP:?set BENCH_PUBLIC_IPS+BENCH_PRIVATE_IPS, or RESOURCE_GROUP for az resolution}"
  PREFIX="${VM_NAME_PREFIX:-jaco}"
  echo "[k3s] resolving node IPs from Azure RG=$RESOURCE_GROUP prefix=$PREFIX"
  mapfile -t PUB < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.publicIpAddresses[0].ipAddress')
  mapfile -t PRIV < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.privateIpAddresses[0]')
fi
[[ ${#PUB[@]} -ge 1 && ${#PUB[@]} -eq ${#PRIV[@]} ]] \
  || { echo "[k3s] could not resolve matching public/private IPs" >&2; exit 1; }

NODE1_PRIV="${PRIV[0]}"
REGISTRY="${REGISTRY:-${NODE1_PRIV}:5000}"
echo "[k3s] nodes:"
for i in "${!PUB[@]}"; do echo "  node$((i+1))  pub=${PUB[$i]}  priv=${PRIV[$i]}"; done
echo "[k3s] registry: $REGISTRY"

ssh_node() { ssh "${SSH_OPTS[@]}" "$SSH_USER@$1" "$2"; }
kc() { ssh_node "${PUB[0]}" "sudo k3s kubectl $1"; }

# --- 1. prepare every node (cloud-init + containerd registry config) --------
for i in "${!PUB[@]}"; do
  pub="${PUB[$i]}"
  echo "[k3s] preparing node$((i+1)) ($pub)"
  scp "${SSH_OPTS[@]}" "$HERE/prepare-node.sh" "$SSH_USER@$pub:/tmp/"
  ssh_node "$pub" "sudo bash /tmp/prepare-node.sh '$REGISTRY'"
done

# --- 2. in-cluster registry + image build/push (docker, on node-1 only) -----
echo "[k3s] installing docker on node-1 (for the build + registry)"
ssh_node "${PUB[0]}" "command -v docker >/dev/null 2>&1 || curl -fsSL https://get.docker.com | sudo sh"
# The build host's docker daemon must treat the in-cluster registry as insecure
# (plain HTTP), or `docker push` fails with "server gave HTTP response to HTTPS
# client". containerd's registries.yaml (prepare-node.sh) only covers k3s pulls;
# this covers node-1's docker pushes.
echo "[k3s] configuring docker insecure-registry ($REGISTRY) on node-1"
ssh_node "${PUB[0]}" "printf '{\"insecure-registries\":[\"%s\"]}\n' '$REGISTRY' | sudo tee /etc/docker/daemon.json >/dev/null && sudo systemctl restart docker"
echo "[k3s] starting registry on node-1"
ssh_node "${PUB[0]}" "sudo docker inspect registry >/dev/null 2>&1 || sudo docker run -d --restart=always --name registry -p 5000:5000 registry:2"

echo "[k3s] shipping workload build contexts to node-1"
ssh_node "${PUB[0]}" "rm -rf ~/k3s-bench && mkdir -p ~/k3s-bench"
scp -r "${SSH_OPTS[@]}" "$SAMPLES_DIR/workload" "$SSH_USER@${PUB[0]}:~/k3s-bench/workload"

echo "[k3s] building + pushing workload images on node-1 -> $REGISTRY"
ssh_node "${PUB[0]}" "
  set -e
  sudo docker build -t '$REGISTRY/bench-web:latest'      ~/k3s-bench/workload/web
  sudo docker build -t '$REGISTRY/bench-api:latest'      ~/k3s-bench/workload/api
  sudo docker build -t '$REGISTRY/bench-postgres:latest' ~/k3s-bench/workload/postgres
  sudo docker push '$REGISTRY/bench-web:latest'
  sudo docker push '$REGISTRY/bench-api:latest'
  sudo docker push '$REGISTRY/bench-postgres:latest'
"

# --- 3. k3s server on node-1 ------------------------------------------------
echo "[k3s] installing k3s server on node-1 (advertise ${NODE1_PRIV})"
ssh_node "${PUB[0]}" "curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC='server --node-ip=${NODE1_PRIV} --advertise-address=${NODE1_PRIV} --tls-san=${NODE1_PRIV} --write-kubeconfig-mode=644' sh -"
echo "[k3s] waiting for the API server"
ssh_node "${PUB[0]}" "until sudo k3s kubectl get --raw=/readyz >/dev/null 2>&1; do sleep 2; done"
TOKEN="$(ssh_node "${PUB[0]}" "sudo cat /var/lib/rancher/k3s/server/node-token")"
[[ -n "$TOKEN" ]] || { echo "[k3s] failed to read node-token" >&2; exit 1; }

# --- 4. k3s agents on node-2/3 ----------------------------------------------
for i in "${!PUB[@]}"; do
  [[ "$i" -eq 0 ]] && continue
  echo "[k3s] joining node$((i+1)) as agent -> https://${NODE1_PRIV}:6443"
  ssh_node "${PUB[$i]}" "curl -sfL https://get.k3s.io | K3S_URL='https://${NODE1_PRIV}:6443' K3S_TOKEN='$TOKEN' INSTALL_K3S_EXEC='agent --node-ip=${PRIV[$i]}' sh -"
done

echo "[k3s] waiting for all ${#PUB[@]} nodes to be Ready"
ssh_node "${PUB[0]}" "
  for _ in \$(seq 1 60); do
    ready=\$(sudo k3s kubectl get nodes --no-headers 2>/dev/null | grep -cw Ready || true)
    [ \"\$ready\" -ge ${#PUB[@]} ] && break
    sleep 5
  done
  sudo k3s kubectl get nodes -o wide
"

# --- 5. deploy the workload --------------------------------------------------
# Rewrite the registry host (manifests ship with the jaco-1:5000 placeholder,
# which Azure VNet doesn't resolve across VMs) to node-1's private IP, which is
# in every node's registries.yaml. Same fix as the JACO/Swarm benches.
echo "[k3s] applying manifests (REGISTRY=$REGISTRY)"
ssh_node "${PUB[0]}" "mkdir -p ~/k3s-bench/manifests"
scp "${SSH_OPTS[@]}" "$K3S_DIR"/manifests/*.yaml "$SSH_USER@${PUB[0]}:~/k3s-bench/manifests/"
ssh_node "${PUB[0]}" "sed -i 's|jaco-1:5000|$REGISTRY|g' ~/k3s-bench/manifests/*.yaml"
# Apply the Traefik ACME patch first so helm-controller has it before we depend
# on HTTPS; then the rest. `apply -f dir` is order-independent for the workload.
ssh_node "${PUB[0]}" "sudo k3s kubectl apply -f ~/k3s-bench/manifests/"

echo "[k3s] waiting for workload rollouts"
for d in redis-primary redis-replica pg-primary pg-replica api web; do
  kc "-n bench rollout status deploy/$d --timeout=180s" || echo "[k3s] WARN: $d not fully rolled out"
done

# ACME for jaco.sh only works once :80 is routable through the LB — the HTTP-01
# challenge target. Traefik requests the cert at startup, which races ahead of
# the LB marking the servicelb backends healthy; if it loses that race it serves
# its self-signed default cert and doesn't promptly retry. Now that the workload
# is up and :80 answers through the LB, bounce Traefik so it re-runs the
# challenge, and wait until the served cert is the (staging) LE cert, so the
# bench measures real ACME-terminated HTTPS like JACO/Swarm.
echo "[k3s] (re)issuing the ACME staging cert for jaco.sh"
until curl -s -o /dev/null --max-time 5 "http://${PUB[0]}/"; do sleep 3; done   # :80 routable
kc "-n kube-system rollout restart deploy/traefik" || true
kc "-n kube-system rollout status deploy/traefik --timeout=120s" || true
issued=0
for _ in $(seq 1 40); do
  issuer="$(echo | openssl s_client -connect "${PUB[0]}:443" -servername jaco.sh 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null || true)"
  case "$issuer" in
    *"TRAEFIK DEFAULT"*|"") sleep 6 ;;
    *) echo "[k3s] ingress cert issued: $issuer"; issued=1; break ;;
  esac
done
[[ "$issued" == "1" ]] || echo "[k3s] WARN: ACME cert not issued in time; ingress is serving Traefik's default cert"

echo
echo "[k3s] done. Traefik terminates TLS for jaco.sh (ACME staging) and routes to"
echo "[k3s]   web; k3s servicelb publishes :80/:443 on all nodes (behind the LB)."
echo "[k3s]   Bench over HTTPS, same as JACO: BENCH_TARGET=https://jaco.sh"
echo "[k3s] check rollout:  ssh $SSH_USER@${PUB[0]} 'sudo k3s kubectl -n bench get pods -o wide'"
