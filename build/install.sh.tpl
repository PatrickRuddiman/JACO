#!/usr/bin/env bash
# build/install.sh.tpl — placeholder installer body. The full installer
# (curl + verify minisig + extract + systemd-enable + bootstrap-vs-join
# branches) lands in task 36; this placeholder lets the release pipeline
# bake the binary tarballs today.
#
# Substitutions performed by build/release.sh:
#   __VERSION__ → release tag (e.g. v0.1.0)

set -euo pipefail

VERSION="__VERSION__"

cat <<EOF
JACO ${VERSION} installer placeholder.

The full installer (signature verification, systemd unit installation,
bootstrap-vs-join handling) lands with task 36. This shipped placeholder
documents the substitution channel + ensures the release pipeline produces
correctly-shaped tarballs.

Manual install steps for now:
  1. Copy ./jaco to /usr/local/bin/jaco (chmod 0755).
  2. Copy ./jaco.service to /etc/systemd/system/jaco.service.
  3. systemctl daemon-reload && systemctl enable --now jaco.service
EOF
