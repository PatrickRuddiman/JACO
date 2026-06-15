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
#
# Exception (upgrades only): a node that already holds committed raft state
# is a cluster member, so an upgrade retroactively re-enables jaco.service if
# it was left disabled. Gated on raft state, so a fresh install never trips
# it. Rationale + the #151 history live with the heal in the upgrade block.

set -e

# Detect whether this is an upgrade vs. a fresh install. dpkg/rpm pass
# the operation in $1 with different conventions:
#   deb  fresh: `configure` with empty $2   upgrade: `configure <old-version>`
#   rpm  fresh: `1`                          upgrade: `2` (or more)
# On an upgrade we restart an already-running unit below; on a fresh
# install we deliberately leave it stopped (see header).
is_upgrade=0
case "$1" in
    configure)
        [ -n "$2" ] && is_upgrade=1
        ;;
    [2-9]|[1-9][0-9]*)
        is_upgrade=1
        ;;
esac

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

    if [ "$is_upgrade" = 1 ]; then
        # Pick up the freshly-installed binary + unit if the operator already
        # had the service enabled and running. preremove.sh no longer stops the
        # unit on upgrade, so "is-active" here reflects the pre-upgrade state.
        if systemctl is-enabled --quiet jaco 2>/dev/null \
           && systemctl is-active --quiet jaco 2>/dev/null; then
            systemctl restart jaco >/dev/null 2>&1 || true
        fi

        # Retroactive #151 heal. A node that ran `cluster init` / `node join`
        # on a release older than the CLI's auto-enable (issue #151) is a
        # committed cluster member whose jaco.service is still *disabled* — and
        # because upgrades deliberately preserve systemd state (preremove.sh),
        # that node stays one reboot away from silently dropping out of the
        # cluster forever. If it holds committed raft state but the unit is
        # disabled, enable it now so the upgrade carries it onto the fix.
        #
        # Gated on raft state at $data_dir/raft/log.db — the exact marker jacod,
        # the bootstrapper, and `cluster init` all treat as "this node is a
        # cluster member" — so it can NEVER fire on a fresh or half-configured
        # install: no committed state, no enable. The deliberate "don't
        # auto-start an uninitialized node" posture (see header) is untouched.
        data_dir=/var/lib/jaco
        if [ -f /etc/jaco/jacod.yaml ]; then
            cfg_dir=$(sed -n 's/^[[:space:]]*data_dir:[[:space:]]*\([^[:space:]]*\).*/\1/p' \
                      /etc/jaco/jacod.yaml 2>/dev/null | head -n1)
            [ -n "$cfg_dir" ] && data_dir=$cfg_dir
        fi
        if [ -f "$data_dir/raft/log.db" ] \
           && ! systemctl is-enabled --quiet jaco 2>/dev/null; then
            if systemctl enable jaco >/dev/null 2>&1; then
                echo "JACO: re-enabled jaco.service on this committed cluster node so it survives reboot (issue #151)."
            fi
        fi
    fi
fi

# Upgrades are quiet — the operator already configured this node. Only
# print the getting-started banner on a fresh install.
if [ "$is_upgrade" = 1 ]; then
    exit 0
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
