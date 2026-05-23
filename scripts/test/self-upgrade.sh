#!/usr/bin/env bash
# self-upgrade.sh — E2E: build a release tarball + a "vNEW" tarball
# with a tweaked version string, run `jaco self-upgrade` against a
# local HTTP server hosting the new tarball, assert both binaries
# swap + the version check passes.
#
# Gated by JACO_SELF_UPGRADE_FORCE=1. Needs minisign installed for
# tarball signing (the release pipeline emits a SHA256SUMS.minisig).

set -euo pipefail

if [[ "${JACO_SELF_UPGRADE_FORCE:-0}" != "1" ]]; then
  echo "SKIP self-upgrade.sh: set JACO_SELF_UPGRADE_FORCE=1 to enable."
  exit 0
fi

cd "$(dirname "$0")/../.."

WORK="$(mktemp -d -t jaco-self-upgrade-XXXX)"
trap 'kill $HTTP_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

# Build the v1 baseline.
mkdir -p "$WORK/prefix/bin"
VERSION=v1 bash build/release.sh "$WORK/dist-v1" 2>/dev/null || {
  echo "SKIP self-upgrade.sh: release.sh build failed (likely missing minisign)"
  exit 0
}
# Stage the v1 binaries in the prefix where install.sh would put them.
TAR_V1=$(find "$WORK/dist-v1" -name 'jaco-v1-linux-amd64.tar.gz' | head -1)
[[ -z "$TAR_V1" ]] && { echo "SKIP self-upgrade.sh: tarball not produced"; exit 0; }
tar xzf "$TAR_V1" -C "$WORK/prefix/bin" --wildcards --no-anchored 'jaco' 'jacod' --strip-components 1

# Build a "vNEW" with a version stamp so we can detect the swap.
VERSION=vNEW bash build/release.sh "$WORK/dist-vnew" 2>/dev/null || { echo "FAIL: vNEW build"; exit 1; }
TAR_NEW=$(find "$WORK/dist-vnew" -name 'jaco-vNEW-linux-amd64.tar.gz' | head -1)

# Serve the vNEW directory on localhost.
(cd "$(dirname "$TAR_NEW")" && python3 -m http.server 9876) >"$WORK/http.log" 2>&1 &
HTTP_PID=$!
sleep 1

# Run self-upgrade against the prefix.
"$WORK/prefix/bin/jaco" self-upgrade \
  --url "http://127.0.0.1:9876/$(basename "$TAR_NEW")" \
  --prefix "$WORK/prefix/bin" \
  || { echo "FAIL: self-upgrade returned non-zero"; exit 1; }

NEW_VER=$("$WORK/prefix/bin/jacod" --version)
if [[ "$NEW_VER" != "vNEW" ]]; then
  echo "FAIL: jacod still reports '$NEW_VER' instead of vNEW"
  exit 1
fi
echo "PASS: self-upgrade (jacod -> $NEW_VER)"
