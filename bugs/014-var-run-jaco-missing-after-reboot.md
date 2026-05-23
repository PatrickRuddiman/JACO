# BUG 014 — /var/run/jaco missing after reboot

## Symptom

After the 2am Azure auto-shutdown + start, jacod refuses to start on
every VM:
```
jacod: gRPC server: mkdir socket parent:
  mkdir /var/run/jaco: permission denied
```
systemctl loops "activating → failed" indefinitely (restart counter
hits 70+ before I notice).

## Severity

**Blocking after any reboot.** Production-fatal — every VM restart
takes the cluster down.

## Root cause

`/var/run` (== `/run`) is tmpfs on Debian; contents are cleared on
every boot. The install path creates `/var/run/jaco` once during
install.sh, but that directory does NOT survive the reboot. On next
boot the daemon tries to mkdir the parent of its unix socket and
fails because `jaco` user can't write to `/var/run/`.

The systemd unit's `ReadWritePaths=/var/run/jaco` doesn't create the
directory — it just allows writes to it once it exists.

## Fix

Add `RuntimeDirectory=jaco` to the systemd unit. systemd creates
`/run/jaco` (== `/var/run/jaco`) at unit start with mode 0755 owned
by the service `User=jaco / Group=jaco`. Cleans it on unit stop.

Also fix the existing VMs by `mkdir + chown` manually before
redeploying the unit.

## Status

**FIXING NOW.**
