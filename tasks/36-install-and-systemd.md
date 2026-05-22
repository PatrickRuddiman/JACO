Parent slice: [packaging](../slices/packaging.md)
Depends on: 35

# Task 36 — install-and-systemd

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Unattended `install.sh` with env overrides, `jaco.service` systemd unit, and `uninstall.sh` clean-removal script.

## Tasks
- [x] Fill out `build/install.sh.tpl`. Reads env: JACO_PREFIX (default `/usr/local`), JACO_DATA_DIR (default `/var/lib/jaco`), JACO_USER (default `jaco`). Detects OS (`uname -s`) + arch (`uname -m`); refuses when the tarball's directory name doesn't match the host. Idempotent: re-running with the same version exits 0 with `jaco $VERSION already installed`; different version upgrades the binary in place (stops the service first if running) and prints `Upgraded from <old> → <new>`.
- [x] install.sh actions: creates the `$JACO_USER` system user via `useradd --system --shell /sbin/nologin --home-dir $JACO_DATA_DIR --no-create-home` when missing; creates `$JACO_DATA_DIR` (0700, owned by jaco:jaco) when missing; installs `$BIN_PATH` mode 0755; installs `jaco.service` to `/etc/systemd/system`; runs `systemctl daemon-reload && systemctl enable jaco` (does NOT start); prints next-step instructions (bootstrap / join / start / journal).
- [x] Fill out `build/jaco.service` with the spec'd shape: `[Unit]` Description + After=network-online.target docker.service + Wants=network-online.target; `[Service]` Type=notify, ExecStart, User/Group=jaco, AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW, NoNewPrivileges, ProtectSystem=strict, ReadWritePaths=/var/lib/jaco /run, Restart=on-failure, RestartSec=5, Environment=JACO_DATA_DIR, LimitNOFILE=65536; `[Install]` WantedBy=multi-user.target.
- [x] Create `build/uninstall.sh`: stops + disables jaco.service, removes the unit + the binary, removes `$JACO_DATA_DIR` (skip with `--preserve-data`), removes the `$JACO_USER` user only when the data dir was removed.
- [x] Update `build/release.sh` to bundle `uninstall.sh` into every release tarball (now 7 entries: `/`, install.sh, jaco, jaco.service, uninstall.sh, LICENSE, README.md).
- [x] Create `scripts/test/install.sh` integration test scaffold. Gated behind `JACO_INSTALL_TEST_FORCE=1` (default skip exits 0 so CI passes until a privileged runner provisioned). Builds the release tarball, extracts it, runs install.sh, asserts `systemctl is-enabled jaco == enabled` + `is-active == inactive`, re-runs to verify the idempotent `already installed` path, then runs uninstall.sh and asserts the four removal conditions.
- [ ] **Deferred**: actually running `scripts/test/install.sh` in a privileged container. Requires CI provisioning of a Linux runner with systemd + useradd + root. The script is ready; the CI plumbing is the missing piece.

## Acceptance criteria
- [x] `build/install.sh.tpl` + `build/jaco.service` + `build/uninstall.sh` exist with the spec'd body; release tarball bundles all three.
- [x] `tar -tzf dist/jaco-test-linux-amd64.tar.gz` lists `install.sh`, `jaco.service`, `uninstall.sh` alongside `jaco`, `LICENSE`, `README.md` (verified).
- [ ] `bash scripts/test/install.sh` exits 0 in a privileged container — deferred; script is ready.
- [ ] Test asserts `systemctl is-enabled jaco == enabled` after install — deferred to privileged-runner CI.
- [ ] Test asserts idempotent re-install prints `already installed` — deferred.
- [ ] Test asserts uninstall removes user + data + binary + unit — deferred.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
