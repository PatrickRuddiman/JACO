---
sources:
  - cmd/jaco/self_upgrade.go
  - internal/packaging/
  - .github/workflows/release.yml
  - build/packaging/
  - nfpm.yaml
---

# Upgrades

JACO is upgraded **one node at a time** with `jaco self-upgrade`. The
command verifies the release tarball (minisign signature over
`SHA256SUMS`, plus SHA-256 of the tarball), atomically swaps both
binaries, restarts the daemon under systemd, and rolls back on health
failure.

CLI reference: [`jaco self-upgrade`](../cli/self-upgrade.md).

If you installed from a `.deb` / `.rpm` you can also upgrade by
installing a newer package over the old one. That path preserves the
node's systemd state — see
[Package-manager upgrades](#package-manager-upgrades-deb--rpm) below.

## Why one node at a time

JACO's design commits to a **single static binary per node** plus a
strict version-skew bound: an N+1 CLI must accept commands against an
N daemon within the same major version. gRPC field additions are
backward-compatible; raft FSM apply is binary-compatible across
adjacent versions. So a rolling upgrade — one node up, wait for
raft rejoin, move to the next — is the canonical path.

Cluster-wide coordinated upgrade (`jaco cluster upgrade --all`) is
explicitly **not** in v1; the operator drives the rotation.

## Walkthrough

For a cluster `{node-1, node-2, node-3}` going from `v0.1.0` to
`v0.2.0`:

### 1. Pick the new release

Confirm the artifact exists at the GitHub release page:

```
https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/jaco-v0.2.0-linux-amd64.tar.gz
https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/SHA256SUMS
https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/SHA256SUMS.minisig
```

`self-upgrade` fetches all three automatically from the `--url` you
pass (the sibling URL pattern replaces the last path segment).

### 2. Upgrade the first node

```sh
ssh node-1
sudo jaco self-upgrade \
  --url https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/jaco-v0.2.0-linux-amd64.tar.gz
```

The command:

1. Downloads tarball + checksums + signature.
2. Verifies the minisign signature against the embedded public key
   (`internal/packaging/release-pubkey.txt`). Any signature mismatch
   aborts before touching binaries.
3. Verifies the SHA-256 of the tarball against `SHA256SUMS`.
4. Extracts `jaco` and `jacod` from the tarball.
5. Saves `/usr/local/bin/jaco` and `/usr/local/bin/jacod` as
   `.prev`.
6. Stages new binaries as `.upgrading`, then atomically renames over
   the live paths.
7. Runs `systemctl restart jacod`.
8. Polls `/usr/local/bin/jacod --version` for up to 3 s.

On health-poll failure, both binaries are restored from `.prev`, the
daemon is restarted again, and the command exits non-zero with
`post-upgrade health check failed; rolled back`.

### 3. Confirm the upgrade

From any other node:

```sh
jaco node list --server $LEADER
```

Wait for `node-1` to appear back as a member (status `READY`). Then
on `node-1` itself:

```sh
jacod --version
jaco cluster status
```

### 4. Advance to the next node

Repeat for `node-2`, then `node-3`. The cluster maintains majority
throughout — a 3-node cluster tolerates one node down — so apply,
status, logs all keep working from the surviving nodes.

## Rolling back a release

A `self-upgrade` that **succeeds** does not preserve `.prev` forever —
the next `self-upgrade` overwrites them. To roll an upgraded cluster
back to the prior version, run `jaco self-upgrade` against the
prior version's tarball URL on each node.

A `self-upgrade` that **fails the post-restart health check** rolls
back automatically on that node only.

## Package-manager upgrades (deb / rpm)

`jaco self-upgrade` is the canonical in-place path, but you can also
upgrade a node by installing a newer package over the older one:

```sh
# Debian / Ubuntu
sudo apt install ./jaco_<X.Y.Z>_<arch>.deb
# RHEL / Fedora
sudo dnf upgrade ./jaco-<X.Y.Z>-1.<arch>.rpm
```

The package preserves the node's systemd state across the upgrade,
matching the standard debhelper `--restart-after-upgrade` posture:

- The pre-remove hook
  ([`build/packaging/preremove.sh`](../../build/packaging/preremove.sh))
  only stops and disables `jaco` on a real removal. dpkg/rpm run the
  pre-remove script on an upgrade too, so it inspects the maintainer
  argument (`remove` / `0` = removal, `upgrade <ver>` / `1` = upgrade)
  and does nothing on upgrade. A node that was `enabled` + `active`
  stays that way.
- The post-install hook
  ([`build/packaging/postinstall.sh`](../../build/packaging/postinstall.sh))
  reloads systemd, and on an upgrade restarts `jaco` **only if** the
  unit was already enabled and running, so the new binary is picked up.

A **fresh** install never auto-enables or auto-starts the service — you
still run `sudo systemctl enable --now jaco` once after editing
`/etc/jaco/jacod.yaml` (see [Installation](../installation.md)). Only
nodes you have already brought up are restarted on upgrade.

Unlike `self-upgrade`, the package path does **not** verify a minisign
signature or roll back on a failed health check. Prefer `self-upgrade`
for routine rolling upgrades; use the package path when a fleet tool
drives `apt` / `dnf` directly.

## On hosts without systemd

The restart step is a soft skip: binaries swap, but starting the new
daemon is the operator's job (`rc-service jacod restart`, manual
respawn, etc.). On Alpine in particular, JACO ships the `.apk` but
relies on the operator to wire whatever supervision they prefer.

## Verification keys

The minisign public key is embedded at build time from
[`internal/packaging/release-pubkey.txt`](../../internal/packaging/release-pubkey.txt).
Rotation requires:

1. Generate a new minisign keypair offline.
2. Publish a new JACO release whose pubkey constant is the new key,
   still signed with the **old** key (operators are running old code).
3. Operators upgrade to that release; their daemons now verify future
   releases with the new key.
4. The next release after that is signed with the new key.

See [release and packaging](../contributing/release-and-packaging.md).

## See also

- [`jaco self-upgrade`](../cli/self-upgrade.md)
- [Release and packaging](../contributing/release-and-packaging.md)
- [Recovery](recovery.md)
- [Cluster lifecycle](../concepts/cluster-lifecycle.md)
