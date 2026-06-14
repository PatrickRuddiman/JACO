#!/bin/sh
# JACO post-install hook (deb / rpm).
#
# Creates the system `jaco` user + group the systemd unit runs as, joins
# that user into the docker group so jacod can dial /var/run/docker.sock,
# then reloads systemd so the freshly-installed unit is picked up. We DO
# NOT enable or start the service here: the operator must edit
# /etc/jaco/jacod.yaml (cluster_addr / listen_addr / data_dir) and then
# run `sudo systemctl enable --now jaco` explicitly. Auto-start on a
# half-configured node would silently come up uninitialized.

set -e

# Create the jaco system group + user if missing. --system gives a sub-1000
# uid/gid and no aging. /var/lib/jaco is the data dir but we don't create
# the home eagerly; RuntimeDirectory= in the unit handles /run/jaco.
if ! getent group jaco >/dev/null 2>&1; then
    groupadd --system jaco
fi
if ! getent passwd jaco >/dev/null 2>&1; then
    useradd --system --gid jaco \
            --home-dir /var/lib/jaco --no-create-home \
            --shell /usr/sbin/nologin \
            jaco
fi

# jacod talks to /var/run/docker.sock; needs the docker group to do that
# unrooted. If docker is installed (it should be — declared as a Depends),
# the group exists.
if getent group docker >/dev/null 2>&1; then
    usermod -aG docker jaco >/dev/null 2>&1 || true
fi

# Ensure the data dir exists and is owned by jaco. The daemon creates it
# lazily on first boot, but pre-creating prevents a permission flap.
install -d -m 0750 -o jaco -g jaco /var/lib/jaco

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
  /lib/systemd/system/jaco.socket   (local-control socket, issue #167)
Service user:
  jaco (system, in docker group)

Next:
  sudo systemctl enable --now jaco
  sudo jaco cluster init        # bootstrap a new cluster
  # or
  sudo jaco node join --peer <host:7000> --token <single-use>
EOF

exit 0
