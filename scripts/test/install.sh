#!/usr/bin/env bash
# scripts/test/install.sh — install / re-install / uninstall smoke test.
#
# Runs in a privileged container or VM where systemctl + useradd work.
# Builds a single-platform tarball, extracts it to a temp dir, runs
# install.sh, asserts enabled + inactive, re-runs to assert idempotency,
# then runs uninstall.sh and asserts removal.
#
# Gated behind JACO_INSTALL_TEST_FORCE=1 — the default skip lets CI pass
# until a privileged runner is provisioned.

set -euo pipefail

if [[ "${JACO_INSTALL_TEST_FORCE:-0}" != "1" ]]; then
  cat >&2 <<'EOF'
install.sh smoke test requires:
  - Privileged execution (systemctl + useradd in the test scope).
  - The release tarball available locally (build via make release first).

Set JACO_INSTALL_TEST_FORCE=1 to run. The default skip exits 0 so CI
passes until a privileged runner is wired up.
EOF
  exit 0
fi

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "install test: must run as root" >&2; exit 1
  fi
}

build_tarball() {
  echo "[test] building release tarball" >&2
  VERSION="${VERSION:-test}" bash build/release.sh >/dev/null
  local os arch tarball
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported arch"; exit 1 ;;
  esac
  tarball="dist/jaco-${VERSION:-test}-${os}-${arch}.tar.gz"
  if [[ ! -f "$tarball" ]]; then
    echo "tarball missing: $tarball" >&2; exit 1
  fi
  echo "$tarball"
}

assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: $msg (got=$got want=$want)" >&2
    exit 1
  fi
  echo "PASS: $msg ($got)"
}

main() {
  require_root
  local tarball stage
  tarball="$(build_tarball)"
  stage="$(mktemp -d)"
  tar -C "$stage" -xzf "$tarball"
  local installer
  installer="$(find "$stage" -maxdepth 2 -name install.sh | head -n1)"

  echo "[test] first install"
  bash "$installer"
  assert_eq "$(systemctl is-enabled jaco 2>/dev/null)" "enabled" "systemctl is-enabled jaco"
  assert_eq "$(systemctl is-active jaco 2>/dev/null || echo inactive)" "inactive" "systemctl is-active jaco"

  echo "[test] second install (idempotent)"
  output="$(bash "$installer" 2>&1 || true)"
  if ! grep -q "already installed" <<<"$output"; then
    echo "FAIL: idempotent run did not print 'already installed'" >&2
    echo "--- output:" >&2; echo "$output" >&2
    exit 1
  fi
  echo "PASS: idempotent second install prints 'already installed'"

  echo "[test] uninstall"
  local uninstaller
  uninstaller="$(find "$stage" -maxdepth 2 -name uninstall.sh | head -n1)"
  if [[ -z "$uninstaller" ]]; then
    # uninstall.sh is shipped alongside the binary in /var/lib/jaco/ or in
    # the release tarball; copy it in for the test.
    uninstaller="$(pwd)/build/uninstall.sh"
  fi
  bash "$uninstaller"

  if id -u jaco >/dev/null 2>&1; then echo "FAIL: jaco user still exists"; exit 1; fi
  if [[ -d /var/lib/jaco ]]; then echo "FAIL: /var/lib/jaco still exists"; exit 1; fi
  if [[ -f /usr/local/bin/jaco ]]; then echo "FAIL: binary still exists"; exit 1; fi
  if [[ -f /etc/systemd/system/jaco.service ]]; then echo "FAIL: unit still exists"; exit 1; fi
  echo "PASS: uninstall removed user + data dir + binary + unit"
}

main "$@"
