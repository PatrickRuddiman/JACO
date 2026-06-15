#!/bin/sh
# JACO pre-remove hook (deb / rpm).
#
# Stop + disable the unit ONLY on a real removal, so we don't leave a
# dangling unit the operator has to clean up by hand. On an upgrade we
# do nothing: the maintainer scripts run prerm before unpacking the new
# version, so tearing the unit down here would silently clear the
# enabled+active state on every upgrade (issue #173). postinstall.sh
# restarts the unit after the new files land if it was running.
#
# dpkg/rpm pass the operation in $1, with different conventions:
#   deb  removal: `remove`        upgrade: `upgrade <new-version>`
#   rpm  removal: `0`             upgrade: `1`
# Best-effort: a failed stop must not abort the uninstall (the binary
# may already be gone in some failure modes).

set -e

case "$1" in
    remove|0)
        # Real removal — tear the unit down.
        if command -v systemctl >/dev/null 2>&1; then
            if systemctl is-active --quiet jaco 2>/dev/null; then
                systemctl stop jaco >/dev/null 2>&1 || true
            fi
            if systemctl is-enabled --quiet jaco 2>/dev/null; then
                systemctl disable jaco >/dev/null 2>&1 || true
            fi
        fi
        ;;
    *)
        # Upgrade (deb `upgrade <ver>`, rpm `1`) or anything else:
        # preserve the operator's last-known systemd state.
        ;;
esac

exit 0
