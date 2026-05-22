Parent slice: [packaging](../slices/packaging.md)
Depends on: 35

# Task 36 — install-and-systemd

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Unattended `install.sh` with env overrides, `jaco.service` systemd unit, and `uninstall.sh` clean-removal script.

## Tasks
- [ ] Fill out `build/install.sh.tpl` (becomes `install.sh` after `__VERSION__` substitution in release): runs `set -euo pipefail`; detects OS (`uname -s`) and arch (`uname -m`); refuses if the tarball was built for a different platform; reads env: `JACO_PREFIX` (default `/usr/local`), `JACO_DATA_DIR` (default `/var/lib/jaco`), `JACO_USER` (default `jaco`).
- [ ] install.sh actions: create `$JACO_USER:$JACO_USER` system user (no login shell) via `useradd -r -s /sbin/nologin -d $JACO_DATA_DIR $JACO_USER` if missing; create `$JACO_DATA_DIR` mode 0700 owned by `$JACO_USER:$JACO_USER`; copy `jaco` to `$JACO_PREFIX/bin/jaco` mode 0755; install `jaco.service` to `/etc/systemd/system/jaco.service`; `systemctl daemon-reload && systemctl enable jaco` (do NOT start); print next-step instructions.
- [ ] Idempotent: re-running same version exits 0 with `jaco vX.Y.Z already installed`. Re-running different version: upgrades binary in place, prints `Upgraded from vA → vB`, does not touch data dir or user.
- [ ] Fill out `build/jaco.service`:
  - `[Unit]` `Description=JACO orchestrator`, `After=network-online.target docker.service`, `Wants=network-online.target`.
  - `[Service]` `Type=notify`, `ExecStart=/usr/local/bin/jaco serve`, `User=jaco`, `Group=jaco`, `AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW`, `NoNewPrivileges=true`, `ProtectSystem=strict`, `ReadWritePaths=/var/lib/jaco /run`, `Restart=on-failure`, `RestartSec=5`, `Environment=JACO_DATA_DIR=/var/lib/jaco`, `LimitNOFILE=65536`.
  - `[Install]` `WantedBy=multi-user.target`.
- [ ] Create `build/uninstall.sh` (also bundled in the release tarball): `systemctl stop jaco && systemctl disable jaco`; remove `/etc/systemd/system/jaco.service`; `systemctl daemon-reload`; remove `$JACO_PREFIX/bin/jaco`; remove `$JACO_DATA_DIR` unless `--preserve-data`; remove `$JACO_USER` only if data dir was removed.
- [ ] Create `scripts/test/install.sh` integration test (run in a privileged container or VM): runs install.sh, asserts `systemctl is-enabled jaco` prints `enabled`, `systemctl is-active jaco` prints `inactive`, re-runs install.sh and asserts the idempotent path; finally runs uninstall.sh and asserts everything is gone.

## Acceptance criteria
- [ ] `bash scripts/test/install.sh` exits 0 in a privileged container.
- [ ] Test asserts `systemctl is-enabled jaco == enabled` after install.
- [ ] Test asserts second install run prints `already installed` and exits 0.
- [ ] After uninstall: `! id -u jaco`, `! test -d /var/lib/jaco`, `! test -f /usr/local/bin/jaco`, `! test -f /etc/systemd/system/jaco.service`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
