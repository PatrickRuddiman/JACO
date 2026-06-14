#!/usr/bin/env bash
# JACO uninstaller. Symmetric counterpart of install.sh:
#   - stops + disables jaco.service
#   - removes /etc/systemd/system/jaco.service and jaco.socket
#   - removes $JACO_PREFIX/bin/{jaco,jacod}
#   - removes $JACO_CONFIG_DIR (jacod.yaml + anything else there)
#   - removes $JACO_DATA_DIR (skip with --preserve-data)
#   - removes the jaco system user (only when data dir is removed)
#
# Env overrides match install.sh (JACO_PREFIX, JACO_DATA_DIR, JACO_USER,
# JACO_CONFIG_DIR).

set -euo pipefail

PREFIX="${JACO_PREFIX:-/usr/local}"
DATA_DIR="${JACO_DATA_DIR:-/var/lib/jaco}"
USER_NAME="${JACO_USER:-jaco}"
CONFIG_DIR="${JACO_CONFIG_DIR:-/etc/jaco}"
JACO_BIN_PATH="$PREFIX/bin/jaco"
JACOD_BIN_PATH="$PREFIX/bin/jacod"
SERVICE_PATH="/etc/systemd/system/jaco.service"
SOCKET_PATH="/etc/systemd/system/jaco.socket"

PRESERVE_DATA=0
for arg in "$@"; do
  case "$arg" in
    --preserve-data) PRESERVE_DATA=1 ;;
    *) echo "uninstall.sh: unknown arg $arg" >&2; exit 1 ;;
  esac
done

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "uninstall.sh: must run as root" >&2
    exit 1
  fi
}

stop_service() {
  if systemctl is-active --quiet jaco 2>/dev/null; then
    systemctl stop jaco
  fi
  if systemctl is-enabled --quiet jaco 2>/dev/null; then
    systemctl disable jaco
  fi
}

remove_unit() {
  local reload=0
  if [[ -f "$SOCKET_PATH" ]]; then
    rm -f "$SOCKET_PATH"
    reload=1
  fi
  if [[ -f "$SERVICE_PATH" ]]; then
    rm -f "$SERVICE_PATH"
    reload=1
  fi
  if [[ "$reload" -eq 1 ]]; then
    systemctl daemon-reload
  fi
}

remove_binaries() {
  for bin in "$JACO_BIN_PATH" "$JACOD_BIN_PATH"; do
    if [[ -f "$bin" ]]; then
      rm -f "$bin"
    fi
  done
}

remove_config() {
  if [[ -d "$CONFIG_DIR" ]]; then
    rm -rf "$CONFIG_DIR"
  fi
}

remove_data() {
  if [[ "$PRESERVE_DATA" -eq 1 ]]; then
    echo "uninstall.sh: --preserve-data set; keeping $DATA_DIR" >&2
    return 1
  fi
  if [[ -d "$DATA_DIR" ]]; then
    rm -rf "$DATA_DIR"
  fi
  return 0
}

remove_user() {
  if id -u "$USER_NAME" >/dev/null 2>&1; then
    userdel "$USER_NAME" 2>/dev/null || true
  fi
}

main() {
  require_root
  stop_service
  remove_unit
  remove_binaries
  remove_config
  if remove_data; then
    remove_user
    echo "JACO uninstalled (data + user removed)."
  else
    echo "JACO uninstalled (data + user preserved)."
  fi
}

main "$@"
