#!/usr/bin/env bash
# JACO uninstaller. Symmetric counterpart of install.sh:
#   - stops + disables jaco.service
#   - removes /etc/systemd/system/jaco.service
#   - removes $JACO_PREFIX/bin/jaco
#   - removes $JACO_DATA_DIR (skip with --preserve-data)
#   - removes the jaco system user (only when data dir is removed)
#
# Env overrides match install.sh (JACO_PREFIX, JACO_DATA_DIR, JACO_USER).

set -euo pipefail

PREFIX="${JACO_PREFIX:-/usr/local}"
DATA_DIR="${JACO_DATA_DIR:-/var/lib/jaco}"
USER_NAME="${JACO_USER:-jaco}"
BIN_PATH="$PREFIX/bin/jaco"
SERVICE_PATH="/etc/systemd/system/jaco.service"

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
  if [[ -f "$SERVICE_PATH" ]]; then
    rm -f "$SERVICE_PATH"
    systemctl daemon-reload
  fi
}

remove_binary() {
  if [[ -f "$BIN_PATH" ]]; then
    rm -f "$BIN_PATH"
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
  remove_binary
  if remove_data; then
    remove_user
    echo "JACO uninstalled (data + user removed)."
  else
    echo "JACO uninstalled (data + user preserved)."
  fi
}

main "$@"
