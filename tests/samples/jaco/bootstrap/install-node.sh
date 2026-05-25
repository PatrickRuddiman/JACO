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

# 0. Wait out the base cloud-init's apt activity so our apt-get calls don't race
#    the dpkg lock on a freshly-booted node. No-op once cloud-init has finished.
if command -v cloud-init >/dev/null 2>&1; then
  echo "[install-node] waiting for cloud-init to finish"
  cloud-init status --wait >/dev/null 2>&1 || true
fi

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
#    docker-ce; installs the systemd unit (jaco.service) + /etc/jaco/jacod.yaml.
echo "[install-node] installing jacod from ${DEB}"
apt-get update -y
apt-get install -y "${DEB}"

# 4. Pin the ACME CA to Let's Encrypt STAGING by default — this is a throwaway
#    test bed and prod LE has tight duplicate-cert limits (5/domain/week). Staging
#    certs aren't browser-trusted (tests use curl -k / k6 insecureSkipTLSVerify),
#    and a non-prod acme_ca also auto-skips JACO's stage-first→prod promotion, so
#    nothing ever hits prod. Override with ACME_CA=... (e.g. the prod directory).
ACME_CA="${ACME_CA:-https://acme-staging-v02.api.letsencrypt.org/directory}"
CONF=/etc/jaco/jacod.yaml
echo "[install-node] pinning acme_ca=${ACME_CA}"
sed -i '/^acme_ca:/d' "$CONF"
echo "acme_ca: ${ACME_CA}" >> "$CONF"

# The package ships the daemon as jaco.service (not jacod.service). Restart so
# the acme_ca change is picked up (jacod does not hot-reload config).
systemctl enable jaco
systemctl restart jaco

echo "[install-node] $(hostname) ready: docker + jacod (uninitialized, acme_ca=${ACME_CA})"
