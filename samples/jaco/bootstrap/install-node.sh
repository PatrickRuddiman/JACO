#!/usr/bin/env bash
# install-node.sh — runs ON a fresh Debian node (shipped there by bootstrap.sh).
# Installs Docker + jacod, wires the in-cluster insecure registry, and starts
# the daemon UNINITIALIZED. Cluster init/join is driven afterwards from the
# operator by bootstrap.sh. Idempotent.
#
# Usage (as root, on the node):
#   sudo bash install-node.sh <deb-path> <registry-host:port> [vnet-cidr]

set -euo pipefail

DEB="${1:?usage: install-node.sh <deb-path> <registry-host:port> [vnet-cidr]}"
REGISTRY_HOST="${2:?registry host:port (e.g. 172.16.0.4:5000)}"
VNET_CIDR="${3:-172.16.0.0/16}"

export DEBIAN_FRONTEND=noninteractive

# 1. Docker (jacod's .deb depends on it). get.docker.com installs docker-ce.
if ! command -v docker >/dev/null 2>&1; then
  echo "[install-node] installing docker"
  curl -fsSL https://get.docker.com | sh
fi

# 2. Allow plain-HTTP pulls from the in-cluster registry. Include the VNet CIDR
#    so pulls by private IP work regardless of hostname resolution.
echo "[install-node] configuring insecure registry ${REGISTRY_HOST}"
install -d -m 0755 /etc/docker
cat >/etc/docker/daemon.json <<EOF
{
  "insecure-registries": ["${REGISTRY_HOST}", "${VNET_CIDR}"]
}
EOF
systemctl enable --now docker
systemctl restart docker

# 3. jacod. apt resolves the docker dependency against the now-installed
#    docker-ce; installs the systemd unit + /etc/jaco/jacod.yaml.
echo "[install-node] installing jacod from ${DEB}"
apt-get update -y
apt-get install -y "${DEB}"
systemctl enable --now jacod

echo "[install-node] $(hostname) ready: docker + jacod (uninitialized)"
