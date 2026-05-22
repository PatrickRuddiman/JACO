#!/usr/bin/env bash
# JACO __VERSION__ installer.
#
# Env overrides (with defaults):
#   JACO_PREFIX    /usr/local         binary install prefix; binary lands at $PREFIX/bin/jaco
#   JACO_DATA_DIR  /var/lib/jaco      persistent data dir (raft store, snapshots, keys)
#   JACO_USER      jaco               system user the daemon runs as
#
# Idempotent: re-running with the same version exits 0 with "already installed";
# re-running with a different version upgrades the binary in place without
# touching the data dir or the user.

set -euo pipefail

VERSION="__VERSION__"
PREFIX="${JACO_PREFIX:-/usr/local}"
DATA_DIR="${JACO_DATA_DIR:-/var/lib/jaco}"
USER_NAME="${JACO_USER:-jaco}"
BIN_PATH="$PREFIX/bin/jaco"
SERVICE_PATH="/etc/systemd/system/jaco.service"

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "install.sh: must run as root" >&2
    exit 1
  fi
}

detect_platform() {
  local os arch want_os want_arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "install.sh: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac

  # The installer tarball is named jaco-$VERSION-$os-$arch; the install.sh
  # ships alongside the binary, so the script's directory name is the
  # platform we built for.
  local self_dir
  self_dir="$(basename "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)")"
  case "$self_dir" in
    jaco-*-${os}-${arch}) ;;
    *)
      # Best-effort sanity check: if the tarball name doesn't match the
      # host, refuse rather than installing the wrong binary.
      want_os="${self_dir##*-${arch}}"
      want_arch="${self_dir##*-}"
      echo "install.sh: tarball platform mismatch — installer dir is $self_dir but host is ${os}-${arch}" >&2
      exit 1
      ;;
  esac
}

ensure_user() {
  if id -u "$USER_NAME" >/dev/null 2>&1; then
    return 0
  fi
  echo "install.sh: creating system user $USER_NAME" >&2
  useradd --system --shell /sbin/nologin --home-dir "$DATA_DIR" --no-create-home "$USER_NAME"
}

ensure_data_dir() {
  if [[ ! -d "$DATA_DIR" ]]; then
    echo "install.sh: creating $DATA_DIR" >&2
    install -d -o "$USER_NAME" -g "$USER_NAME" -m 0700 "$DATA_DIR"
  else
    chown "$USER_NAME:$USER_NAME" "$DATA_DIR"
    chmod 0700 "$DATA_DIR"
  fi
}

install_binary() {
  local self_dir
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ ! -x "$self_dir/jaco" ]]; then
    echo "install.sh: jaco binary missing in $self_dir" >&2
    exit 1
  fi

  # Idempotency: same version → exit 0; different version → upgrade.
  if [[ -x "$BIN_PATH" ]]; then
    local current
    current="$($BIN_PATH --version 2>/dev/null || true)"
    if [[ "$current" == *"$VERSION"* ]]; then
      echo "install.sh: jaco $VERSION already installed at $BIN_PATH"
      return 1 # signal: no further work needed
    fi
    echo "install.sh: upgrading from $current → $VERSION" >&2
    if systemctl is-active --quiet jaco 2>/dev/null; then
      systemctl stop jaco
    fi
  fi

  install -m 0755 "$self_dir/jaco" "$BIN_PATH"
}

install_unit() {
  local self_dir
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ ! -f "$self_dir/jaco.service" ]]; then
    echo "install.sh: jaco.service missing in $self_dir" >&2
    exit 1
  fi
  install -m 0644 "$self_dir/jaco.service" "$SERVICE_PATH"
  systemctl daemon-reload
  systemctl enable jaco >/dev/null
}

main() {
  require_root
  detect_platform
  ensure_user
  ensure_data_dir
  # install_binary returns 1 when nothing was done (already installed).
  if install_binary; then
    install_unit
    cat <<EOF
JACO $VERSION installed.

Next:
  - Bootstrap the first node:    sudo -u $USER_NAME $BIN_PATH bootstrap --data-dir $DATA_DIR
  - Or join an existing cluster: sudo -u $USER_NAME $BIN_PATH node join --token <join-token> --peer <addr>
  - Start the daemon:            systemctl start jaco
  - Tail logs:                   journalctl -u jaco -f
EOF
  fi
}

main "$@"
