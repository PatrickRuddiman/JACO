#!/usr/bin/env bash
# prepare-node.sh — runs on EVERY kubeadm node (control-plane + workers) as root,
# before the cluster is formed. Installs the container runtime + kubeadm toolchain
# and applies the kernel/sysctl prereqs kubeadm's preflight requires. containerd
# comes from the Docker apt repo so node-1 can reuse it for the image build.
set -euo pipefail

REGISTRY="${1:?usage: prepare-node.sh <registry-host:port> [k8s-minor]}"
K8S_MINOR="${2:-v1.31}"

if command -v cloud-init >/dev/null 2>&1; then
  cloud-init status --wait >/dev/null 2>&1 || true
fi

export DEBIAN_FRONTEND=noninteractive

# Drop any stale k8s apt list from a previous run: it would make the *next*
# `apt-get update` (below, for containerd) fail signature verification before we
# get a chance to rewrite it, since Trixie's sqv rejects that repo's signature.
rm -f /etc/apt/sources.list.d/kubernetes.list

# --- kernel modules + sysctl (kubeadm preflight) ----------------------------
cat >/etc/modules-load.d/k8s.conf <<'EOF'
overlay
br_netfilter
EOF
modprobe overlay || true
modprobe br_netfilter || true
cat >/etc/sysctl.d/99-k8s.conf <<'EOF'
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system >/dev/null
# kubelet refuses to start with swap on (kubeadm preflight). Azure Debian has
# none by default, but be sure.
swapoff -a || true
sed -i.bak '/\sswap\s/s/^/#/' /etc/fstab 2>/dev/null || true

apt-get update -y
# conntrack + socat + ethtool are kubeadm preflight requirements that the minimal
# Debian base image doesn't ship (conntrack for kube-proxy, socat for
# port-forward). Install alongside the apt/https helpers.
apt-get install -y apt-transport-https ca-certificates curl gnupg conntrack socat ethtool

# --- containerd.io (Docker repo) --------------------------------------------
install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
  curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
fi
chmod a+r /etc/apt/keyrings/docker.gpg
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian ${VERSION_CODENAME} stable" \
  >/etc/apt/sources.list.d/docker.list
apt-get update -y
apt-get install -y containerd.io

# containerd config: SystemdCgroup (kubelet default cgroup driver) + an insecure
# HTTP mirror for the in-cluster registry so k8s can pull the custom images.
containerd config default >/etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
# Point the CRI image registry at certs.d (the hosts.toml below). containerd 2.x
# (config v3) writes `config_path = ''` (single quotes) under
# io.containerd.cri.v1.images.registry; 1.x used double quotes under
# io.containerd.grpc.v1.cri.registry. Scope the edit to that registry section so
# the unrelated transfer plugin's config_path is left untouched.
sed -i -E "/(cri\.v1\.images'|grpc\.v1\.cri\")\.registry\]/,/^[[:space:]]*\[plugins/ s#config_path = (''|\"\")#config_path = \"/etc/containerd/certs.d\"#" /etc/containerd/config.toml
install -d "/etc/containerd/certs.d/${REGISTRY}"
cat >"/etc/containerd/certs.d/${REGISTRY}/hosts.toml" <<EOF
server = "http://${REGISTRY}"
[host."http://${REGISTRY}"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
EOF
systemctl restart containerd
systemctl enable containerd >/dev/null 2>&1 || true

# --- kubeadm / kubelet / kubectl --------------------------------------------
# Debian Trixie verifies apt repo signatures with Sequoia (sqv), whose default
# policy rejects the k8s community repo's InRelease signature as of 2026-02-01
# ("Signature Packet v3 is not considered secure"). This is a throwaway bench, so
# crypto policy — the Docker + Debian repos verify fine and are left signed.
# [trusted=yes] alone doesn't suppress sqv's hard error on this apt, so pair it
# with AllowInsecureRepositories on update + --allow-unauthenticated on install.
echo "deb [trusted=yes] https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/deb/ /" \
  >/etc/apt/sources.list.d/kubernetes.list
apt-get update -y -o Acquire::AllowInsecureRepositories=true
apt-get install -y --allow-unauthenticated kubelet kubeadm kubectl
apt-mark hold kubelet kubeadm kubectl
systemctl enable kubelet >/dev/null 2>&1 || true

echo "[k8s] node prepared (containerd + kubeadm ${K8S_MINOR}, registry ${REGISTRY})"
