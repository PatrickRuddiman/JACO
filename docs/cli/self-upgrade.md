---
sources:
  - cmd/jaco/self_upgrade.go
  - internal/packaging/
---

# `jaco self-upgrade`

Verify + atomically swap the local `jaco` and `jacod` binaries from a
release tarball, restart the daemon, and roll back automatically if the
new daemon fails to report `--version` within three seconds.

## Synopsis

```
sudo jaco self-upgrade --url <https://…/jaco-vX.Y.Z-linux-<arch>.tar.gz> [--prefix <dir>]
```

## Flags

| flag                  | default              | meaning                                  |
|-----------------------|----------------------|------------------------------------------|
| `--url <url>`         | — (required)         | release tarball URL                      |
| `--prefix <dir>`      | `/usr/local/bin`     | directory holding `jaco` + `jacod`       |

## Auth

Local; run as root on the node being upgraded. No cluster RPCs.

## Behavior

1. Downloads the tarball plus its sibling `SHA256SUMS` and
   `SHA256SUMS.minisig` (same base URL, last path segment replaced).
2. Verifies the minisign signature against the embedded public key
   (`internal/packaging/release-pubkey.txt`), then verifies the SHA-256
   of the tarball against `SHA256SUMS`. **Any verification failure
   aborts before touching either binary.**
3. Extracts the `jaco` and `jacod` entries from the tarball.
4. Saves the existing binaries as `<bin>.prev`.
5. Stages the new binaries as `<bin>.upgrading`, then atomically renames
   them over the live paths. Both renames run back-to-back; on the
   second failing, the first is rolled back.
6. If `systemctl` is on PATH, runs `systemctl restart jacod` and polls
   `<prefix>/jacod --version` for up to three seconds.
7. On health-poll failure, restores `<bin>.prev` for both binaries and
   issues one more `systemctl restart jacod`. The command returns
   non-zero with `post-upgrade health check failed; rolled back`.

On hosts without `systemctl` (CI containers, developer machines), the
restart step is a soft skip: binaries swap, but starting the new daemon
is the operator's responsibility.

The verification public key rotates only via a new JACO release built
under the prior key; see
[release and packaging](../contributing/release-and-packaging.md).

## Exit codes

- `0` — upgrade complete and the new daemon reports `--version`.
- `1` — any of: download failure, verification failure (signature or
  checksum), rename failure, post-restart health failure (with
  automatic rollback).

## Examples

Roll a single node forward to v0.2.0:

```sh
sudo jaco self-upgrade \
  --url https://github.com/PatrickRuddiman/JACO/releases/download/v0.2.0/jaco-v0.2.0-linux-amd64.tar.gz
```

Operate the cluster one node at a time. After each upgrade, verify
from any other node:

```sh
jaco node list --server $LEADER
jaco cluster status            # on the upgraded node
```

## See also

- [Upgrades walkthrough](../operations/upgrades.md)
- [Release and packaging](../contributing/release-and-packaging.md)
