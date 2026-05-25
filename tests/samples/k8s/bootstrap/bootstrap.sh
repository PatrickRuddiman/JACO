#!/usr/bin/env bash
# bootstrap.sh — stand up a 3-node Kubernetes cluster with kubeadm on the testbed
# and deploy the shared bench workload. Runs from the OPERATOR host. Mirrors the
# swarm/jaco/k3s flow (prep → registry → build/push → form cluster → deploy); the
# differences are kubeadm init/join + a CNI + ingress-nginx/cert-manager (via
# Helm) instead of a bundled ingress, then `kubectl apply`.
#
# Node addressing (node-1 first = the control-plane) comes from either:
#   * env: BENCH_PUBLIC_IPS / BENCH_PRIVATE_IPS (space-separated, same order)
#   * or Azure: RESOURCE_GROUP + VM_NAME_PREFIX (resolved via `az`)
#
# Env: SSH_USER (azureuser), SSH_KEY (default the bed key), REGISTRY
# (default <node-1 private IP>:5000), K8S_MINOR (v1.31), POD_CIDR (10.244.0.0/16).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAMPLES_DIR="$(cd "$HERE/../.." && pwd)"
REPO_ROOT="$(cd "$SAMPLES_DIR/../.." && pwd)"
K8S_DIR="$SAMPLES_DIR/k8s"

SSH_USER="${SSH_USER:-azureuser}"
_bed_key="$REPO_ROOT/tests/testbed/.ssh/jaco"
SSH_KEY="${SSH_KEY:-$([ -f "$_bed_key" ] && echo "$_bed_key" || echo "$HOME/.ssh/jaco")}"
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o UserKnownHostsFile=/dev/null)
K8S_MINOR="${K8S_MINOR:-v1.31}"
POD_CIDR="${POD_CIDR:-10.244.0.0/16}"

# --- resolve node addresses (node-1 first) ----------------------------------
read -r -a PUB <<<"${BENCH_PUBLIC_IPS:-}"
read -r -a PRIV <<<"${BENCH_PRIVATE_IPS:-}"
if [[ ${#PUB[@]} -eq 0 || ${#PRIV[@]} -eq 0 ]]; then
  : "${RESOURCE_GROUP:?set BENCH_PUBLIC_IPS+BENCH_PRIVATE_IPS, or RESOURCE_GROUP for az resolution}"
  PREFIX="${VM_NAME_PREFIX:-jaco}"
  echo "[k8s] resolving node IPs from Azure RG=$RESOURCE_GROUP prefix=$PREFIX"
  mapfile -t PUB < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.publicIpAddresses[0].ipAddress')
  mapfile -t PRIV < <(az vm list-ip-addresses -g "$RESOURCE_GROUP" -o json \
    | jq -r --arg p "$PREFIX" '[.[] | select(.virtualMachine.name|startswith($p))] | sort_by(.virtualMachine.name)[] | .virtualMachine.network.privateIpAddresses[0]')
fi
[[ ${#PUB[@]} -ge 1 && ${#PUB[@]} -eq ${#PRIV[@]} ]] \
  || { echo "[k8s] could not resolve matching public/private IPs" >&2; exit 1; }

NODE1_PRIV="${PRIV[0]}"
REGISTRY="${REGISTRY:-${NODE1_PRIV}:5000}"
echo "[k8s] nodes:"
for i in "${!PUB[@]}"; do echo "  node$((i+1))  pub=${PUB[$i]}  priv=${PRIV[$i]}"; done
echo "[k8s] registry: $REGISTRY  k8s: $K8S_MINOR  pod-cidr: $POD_CIDR"

ssh_node() { ssh "${SSH_OPTS[@]}" "$SSH_USER@$1" "$2"; }
# kubectl on the control-plane via the admin kubeconfig.
kc() { ssh_node "${PUB[0]}" "sudo kubectl --kubeconfig /etc/kubernetes/admin.conf $1"; }
helm1() { ssh_node "${PUB[0]}" "sudo KUBECONFIG=/etc/kubernetes/admin.conf helm $1"; }

# --- 1. prepare every node (runtime + kubeadm toolchain + prereqs) ----------
for i in "${!PUB[@]}"; do
  pub="${PUB[$i]}"
  echo "[k8s] preparing node$((i+1)) ($pub)"
  scp "${SSH_OPTS[@]}" "$HERE/prepare-node.sh" "$SSH_USER@$pub:/tmp/"
  ssh_node "$pub" "sudo bash /tmp/prepare-node.sh '$REGISTRY' '$K8S_MINOR'"
done

# --- 2. in-cluster registry + image build/push (docker, on node-1 only) -----
echo "[k8s] installing docker on node-1 (reuses containerd.io; for build + registry)"
ssh_node "${PUB[0]}" "command -v docker >/dev/null 2>&1 || sudo apt-get install -y docker-ce docker-ce-cli docker-buildx-plugin"
# docker's daemon must treat the in-cluster registry as insecure (HTTP) for the push.
ssh_node "${PUB[0]}" "printf '{\"insecure-registries\":[\"%s\"]}\n' '$REGISTRY' | sudo tee /etc/docker/daemon.json >/dev/null && sudo systemctl restart docker"
echo "[k8s] starting registry on node-1"
ssh_node "${PUB[0]}" "sudo docker inspect registry >/dev/null 2>&1 || sudo docker run -d --restart=always --name registry -p 5000:5000 registry:2"

echo "[k8s] shipping workload build contexts to node-1"
ssh_node "${PUB[0]}" "rm -rf ~/k8s-bench && mkdir -p ~/k8s-bench"
scp -r "${SSH_OPTS[@]}" "$SAMPLES_DIR/workload" "$SSH_USER@${PUB[0]}:~/k8s-bench/workload"

echo "[k8s] building + pushing workload images on node-1 -> $REGISTRY"
ssh_node "${PUB[0]}" "
  set -e
  sudo docker build -t '$REGISTRY/bench-web:latest'      ~/k8s-bench/workload/web
  sudo docker build -t '$REGISTRY/bench-api:latest'      ~/k8s-bench/workload/api
  sudo docker build -t '$REGISTRY/bench-postgres:latest' ~/k8s-bench/workload/postgres
  sudo docker push '$REGISTRY/bench-web:latest'
  sudo docker push '$REGISTRY/bench-api:latest'
  sudo docker push '$REGISTRY/bench-postgres:latest'
"

# --- 3. kubeadm init on node-1 ----------------------------------------------
echo "[k8s] kubeadm init on node-1 (advertise ${NODE1_PRIV}, pod-cidr ${POD_CIDR})"
ssh_node "${PUB[0]}" "
  set -e
  if ! sudo test -f /etc/kubernetes/admin.conf; then
    sudo kubeadm init --pod-network-cidr='${POD_CIDR}' \
      --apiserver-advertise-address='${NODE1_PRIV}' \
      --cri-socket unix:///run/containerd/containerd.sock
  fi
"
echo "[k8s] waiting for the API server"
ssh_node "${PUB[0]}" "until sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get --raw=/readyz >/dev/null 2>&1; do sleep 3; done"

# --- 4. CNI (flannel) -------------------------------------------------------
echo "[k8s] installing flannel CNI"
kc "apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml"

# --- 5. join workers --------------------------------------------------------
JOIN="$(ssh_node "${PUB[0]}" "sudo kubeadm token create --print-join-command")"
[[ -n "$JOIN" ]] || { echo "[k8s] failed to get join command" >&2; exit 1; }
for i in "${!PUB[@]}"; do
  [[ "$i" -eq 0 ]] && continue
  echo "[k8s] joining node$((i+1)) as worker"
  ssh_node "${PUB[$i]}" "sudo ${JOIN} --cri-socket unix:///run/containerd/containerd.sock >/dev/null 2>&1 || sudo ${JOIN} --cri-socket unix:///run/containerd/containerd.sock"
done

echo "[k8s] waiting for all ${#PUB[@]} nodes to be Ready"
ssh_node "${PUB[0]}" "
  for _ in \$(seq 1 60); do
    ready=\$(sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes --no-headers 2>/dev/null | grep -cw Ready || true)
    [ \"\$ready\" -ge ${#PUB[@]} ] && break
    sleep 5
  done
  sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide
"

# Untaint the control-plane so it also runs workload pods — JACO/Swarm/k3s all
# schedule on every node, so for parity this 3-node cluster must too (otherwise
# kubeadm would run the workload on only 2 of the 3 nodes).
echo "[k8s] untainting control-plane for workload parity (all 3 nodes schedulable)"
kc "taint nodes --all node-role.kubernetes.io/control-plane- 2>/dev/null" || true

# --- 6. ingress-nginx + cert-manager (Helm) ---------------------------------
echo "[k8s] installing helm on node-1"
ssh_node "${PUB[0]}" "command -v helm >/dev/null 2>&1 || curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | sudo bash"
helm1 "repo add ingress-nginx https://kubernetes.github.io/ingress-nginx >/dev/null 2>&1 || true"
helm1 "repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true"
helm1 "repo update >/dev/null"

echo "[k8s] installing ingress-nginx (hostNetwork DaemonSet, behind the LB on :80/:443)"
helm1 "upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx --create-namespace \
  --set controller.kind=DaemonSet \
  --set controller.hostNetwork=true \
  --set controller.dnsPolicy=ClusterFirstWithHostNet \
  --set controller.service.type=ClusterIP \
  --set controller.publishService.enabled=false \
  --wait --timeout 5m"

echo "[k8s] installing cert-manager (with CRDs)"
helm1 "upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set installCRDs=true \
  --wait --timeout 5m"

# --- 7. deploy the workload --------------------------------------------------
echo "[k8s] applying manifests (REGISTRY=$REGISTRY)"
ssh_node "${PUB[0]}" "mkdir -p ~/k8s-bench/manifests"
scp "${SSH_OPTS[@]}" "$K8S_DIR"/manifests/*.yaml "$SSH_USER@${PUB[0]}:~/k8s-bench/manifests/"
ssh_node "${PUB[0]}" "sed -i 's|jaco-1:5000|$REGISTRY|g' ~/k8s-bench/manifests/*.yaml"
kc "apply -f ~/k8s-bench/manifests/"

echo "[k8s] waiting for workload rollouts"
for d in redis-primary redis-replica pg-primary pg-replica api web; do
  kc "-n bench rollout status deploy/$d --timeout=180s" || echo "[k8s] WARN: $d not fully rolled out"
done

# --- 8. wait for cert-manager to issue the staging cert ---------------------
# cert-manager solves HTTP-01 through ingress-nginx; the challenge needs :80
# routable through the LB, which it is once the controller DaemonSet is up.
# cert-manager retries on its own — just wait until the served cert is the LE
# (staging) cert so the bench measures real ACME-terminated HTTPS.
echo "[k8s] waiting for the ACME staging cert for jaco.sh"
until curl -s -o /dev/null --max-time 5 "http://${PUB[0]}/"; do sleep 3; done
issued=0
for _ in $(seq 1 50); do
  issuer="$(echo | openssl s_client -connect "${PUB[0]}:443" -servername jaco.sh 2>/dev/null | openssl x509 -noout -issuer 2>/dev/null || true)"
  case "$issuer" in
    *"Let's Encrypt"*|*"Fake LE"*|*"STAGING"*) echo "[k8s] ingress cert issued: $issuer"; issued=1; break ;;
    *) sleep 6 ;;
  esac
done
[[ "$issued" == "1" ]] || echo "[k8s] WARN: ACME cert not issued in time; ingress may be serving a self-signed cert"

echo
echo "[k8s] done. ingress-nginx terminates TLS for jaco.sh (cert-manager, ACME"
echo "[k8s]   staging) and routes to web; the controller DaemonSet binds :80/:443"
echo "[k8s]   on every node behind the LB. Bench over HTTPS: BENCH_TARGET=https://jaco.sh"
echo "[k8s] check rollout:  ssh $SSH_USER@${PUB[0]} 'sudo kubectl -n bench get pods -o wide'"
