#!/bin/sh
# JACO post-remove hook (deb / rpm).
#
# Reload systemd after the unit file has been deleted. We deliberately
# DO NOT touch /var/lib/jaco (raft state, snapshots, keys) or
# /etc/jaco — operators frequently want to upgrade by removing then
# reinstalling, and that workflow must preserve cluster state.
# Use `purge` workflows or manual rm to clean those directories.

set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0
