#!/bin/sh
# JACO post-install hook (deb / rpm).
#
# Reload systemd if it's present, so the freshly-installed
# /lib/systemd/system/jaco.service is picked up. We DO NOT enable or
# start the service here: the operator must edit
# /etc/jaco/jacod.yaml (cluster_addr / listen_addr / data_dir) and
# then run `sudo systemctl enable --now jaco` explicitly. Auto-start
# on a half-configured node would silently come up uninitialized.

set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'
JACO installed.

Binaries:
  /usr/local/bin/jaco        (CLI client)
  /usr/local/bin/jacod       (daemon)
Config:
  /etc/jaco/jacod.yaml       (edit before first start)
Unit:
  /lib/systemd/system/jaco.service

Next:
  sudo systemctl enable --now jaco
  sudo jaco cluster init        # bootstrap a new cluster
  # or
  sudo jaco node join --peer <host:7000> --token <single-use>
EOF

exit 0
