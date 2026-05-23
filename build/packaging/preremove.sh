#!/bin/sh
# JACO pre-remove hook (deb / rpm).
#
# Stop + disable the unit before the package contents are torn out,
# so we don't end up with a dangling unit file the operator has to
# clean up by hand. Best-effort: a failed stop must not abort the
# uninstall (the binary may already be gone in some failure modes).

set -e

if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet jaco 2>/dev/null; then
        systemctl stop jaco >/dev/null 2>&1 || true
    fi
    if systemctl is-enabled --quiet jaco 2>/dev/null; then
        systemctl disable jaco >/dev/null 2>&1 || true
    fi
fi

exit 0
