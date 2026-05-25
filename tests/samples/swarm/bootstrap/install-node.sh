#!/usr/bin/env bash
# install-node.sh — runs ON a fresh Debian node (shipped there by bootstrap.sh).
# Installs Docker and wires the in-cluster insecure registry. Swarm init/join is
# driven afterwards from the operator by bootstrap.sh. Idempotent.
#
# Usage (as root, on the node):
#   sudo bash install-node.sh <registry-host:port> [vnet-cidr]

set -euo pipefail

REGISTRY_HOST="${1:?usage: install-node.sh <registry-host:port> [vnet-cidr]}"
VNET_CIDR="${2:-172.16.0.0/16}"

export DEBIAN_FRONTEND=noninteractive

# Wait out the base cloud-init's apt activity so our apt-get calls don't race
# the dpkg lock on a freshly-booted node. No-op once cloud-init has finished.
if command -v cloud-init >/dev/null 2>&1; then
  echo "[install-node] waiting for cloud-init to finish"
  cloud-init status --wait >/dev/null 2>&1 || true
fi

# Docker via the official convenience script.
if ! command -v docker >/dev/null 2>&1; then
  echo "[install-node] installing docker"
  curl -fsSL https://get.docker.com | sh
fi

# Allow plain-HTTP pulls from the in-cluster registry (hostname + VNet CIDR so
# pulls by private IP work regardless of name resolution).
echo "[install-node] configuring insecure registry ${REGISTRY_HOST}"
install -d -m 0755 /etc/docker
cat >/etc/docker/daemon.json <<EOF
{
  "insecure-registries": ["${REGISTRY_HOST}", "${VNET_CIDR}"]
}
EOF
systemctl enable --now docker
systemctl restart docker

echo "[install-node] $(hostname) ready: docker + insecure registry"
