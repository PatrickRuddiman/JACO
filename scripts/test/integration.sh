#!/usr/bin/env bash
# Runs JACO's build-tagged integration tests in sequence. Each tag
# exercises a real engine (docker / nftables / wireguard / acme), so
# the script needs root or matching capabilities (CAP_NET_ADMIN,
# CAP_NET_BIND_SERVICE, etc.).
#
# Default behavior is "run every tag whose corresponding env var is
# unset → ALL skip". The script is intended to run on a privileged CI
# runner where every JACO_INTEGRATION_* var is set explicitly.
#
# Usage:
#   JACO_INTEGRATION_DOCKER=1 \
#   JACO_INTEGRATION_NFTABLES=1 \
#   JACO_INTEGRATION_WG=1 \
#   JACO_INTEGRATION_PEBBLE=https://pebble:14000/dir \
#   bash scripts/test/integration.sh

set -euo pipefail

cd "$(dirname "$0")/../.."

declare -a tags=(
  "docker:./internal/runtime/lifecycle/..."
  "nftables:./internal/discovery/firewall/..."
  "wireguard:./internal/discovery/wgmesh/..."
  "acme:./internal/ingress/..."
)

failures=0

for entry in "${tags[@]}"; do
  tag="${entry%%:*}"
  pkg="${entry#*:}"
  echo "--- go test -tags ${tag} ${pkg}"
  if ! go test -tags "${tag}" -race -count=1 -timeout=120s "${pkg}"; then
    failures=$((failures + 1))
    echo "FAIL: -tags ${tag} ${pkg}"
  fi
done

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} integration suite(s) failed"
  exit 1
fi
echo "All integration suites passed (skipped suites count as passes)"
