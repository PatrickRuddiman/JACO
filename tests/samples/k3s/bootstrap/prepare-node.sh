#!/usr/bin/env bash
# prepare-node.sh — runs on EVERY k3s node (server + agents) as root, before k3s
# is installed. Waits out cloud-init (so apt/dpkg is free) and writes the
# containerd registry config so k3s pulls the in-cluster registry over plain
# HTTP. Must exist before `k3s` starts; if written later, k3s needs a restart.
set -euo pipefail

REGISTRY="${1:?usage: prepare-node.sh <registry-host:port>}"

# Let cloud-init settle so package/dpkg locks are free (mirrors the JACO/Swarm
# bootstraps).
if command -v cloud-init >/dev/null 2>&1; then
  cloud-init status --wait >/dev/null 2>&1 || true
fi

install -d -m 0755 /etc/rancher/k3s
cat >/etc/rancher/k3s/registries.yaml <<EOF
mirrors:
  "${REGISTRY}":
    endpoint:
      - "http://${REGISTRY}"
EOF
echo "[k3s] wrote /etc/rancher/k3s/registries.yaml (insecure http for ${REGISTRY})"
