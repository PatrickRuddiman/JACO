#!/usr/bin/env bash
# JACO __VERSION__ installer.
#
# Env overrides (with defaults):
#   JACO_PREFIX     /usr/local       binary install prefix; binaries land at $PREFIX/bin/{jaco,jacod}
#   JACO_DATA_DIR   /var/lib/jaco    persistent data dir (raft store, snapshots, keys)
#   JACO_USER       jaco             system user the daemon runs as
#   JACO_CONFIG_DIR /etc/jaco        config dir; jacod.yaml lands here
#
# Idempotent: re-running with the same version exits 0 with "already installed";
# re-running with a different version upgrades both binaries in place without
# touching the data dir, config, or user.

set -euo pipefail

VERSION="__VERSION__"
PREFIX="${JACO_PREFIX:-/usr/local}"
DATA_DIR="${JACO_DATA_DIR:-/var/lib/jaco}"
USER_NAME="${JACO_USER:-jaco}"
CONFIG_DIR="${JACO_CONFIG_DIR:-/etc/jaco}"
JACO_BIN_PATH="$PREFIX/bin/jaco"
JACOD_BIN_PATH="$PREFIX/bin/jacod"
CONFIG_PATH="$CONFIG_DIR/jacod.yaml"
SERVICE_PATH="/etc/systemd/system/jaco.service"
SOCKET_PATH="/etc/systemd/system/jaco.socket"

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "install.sh: must run as root" >&2
    exit 1
  fi
}

detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "install.sh: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac

  local self_dir
  self_dir="$(basename "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)")"
  case "$self_dir" in
    jaco-*-${os}-${arch}) ;;
    *)
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

ensure_config_dir() {
  if [[ ! -d "$CONFIG_DIR" ]]; then
    install -d -m 0755 "$CONFIG_DIR"
  fi
}

install_binaries() {
  local self_dir
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  for b in jaco jacod; do
    if [[ ! -x "$self_dir/$b" ]]; then
      echo "install.sh: $b binary missing in $self_dir" >&2
      exit 1
    fi
  done

  # Idempotency: query jacod (the long-running binary) — same version → exit 0.
  if [[ -x "$JACOD_BIN_PATH" ]]; then
    local current
    current="$($JACOD_BIN_PATH --version 2>/dev/null || true)"
    if [[ "$current" == *"$VERSION"* ]]; then
      echo "install.sh: jacod $VERSION already installed at $JACOD_BIN_PATH"
      return 1 # signal: no further work needed
    fi
    echo "install.sh: upgrading from $current → $VERSION" >&2
    if systemctl is-active --quiet jaco 2>/dev/null; then
      systemctl stop jaco
    fi
  fi

  install -m 0755 "$self_dir/jaco"  "$JACO_BIN_PATH"
  install -m 0755 "$self_dir/jacod" "$JACOD_BIN_PATH"
}

install_config() {
  local self_dir
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ -f "$CONFIG_PATH" ]]; then
    return 0
  fi
  if [[ ! -f "$self_dir/jacod.yaml" ]]; then
    echo "install.sh: jacod.yaml template missing in $self_dir" >&2
    exit 1
  fi
  install -m 0644 "$self_dir/jacod.yaml" "$CONFIG_PATH"
}

install_unit() {
  local self_dir
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  install -m 0644 "$self_dir/jaco.service" "$SERVICE_PATH"
  # jaco.socket is pulled in by jaco.service's Requires= (issue #167): systemd
  # creates and binds the local-control socket in the host namespace and hands
  # jacod the fd, so it stays reachable by the jaco group. Must ship alongside
  # the service or `systemctl enable jaco` fails on the missing dependency.
  install -m 0644 "$self_dir/jaco.socket" "$SOCKET_PATH"
  systemctl daemon-reload
  systemctl enable jaco >/dev/null
}

main() {
  require_root
  detect_platform
  ensure_user
  ensure_data_dir
  ensure_config_dir
  if install_binaries; then
    install_config
    install_unit
    cat <<EOF
JACO $VERSION installed.

Binaries:
  $JACO_BIN_PATH        (CLI client)
  $JACOD_BIN_PATH       (daemon)
Config:
  $CONFIG_PATH

Next:
  - Start the daemon:           systemctl start jaco
  - Initialize a new cluster:   sudo jaco cluster init
  - Or join an existing one:    sudo jaco node join --peer <host:7000> --token <single-use>
  - Tail logs:                  journalctl -u jaco -f
  - Check status:               jaco cluster status
EOF
  fi
}

main "$@"
