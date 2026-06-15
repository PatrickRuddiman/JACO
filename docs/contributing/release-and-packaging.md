---
sources:
  - .github/workflows/release.yml
  - nfpm.yaml
  - Makefile
  - internal/packaging/
  - build/packaging/
  - cmd/jaco/self_upgrade.go
  - build/jaco.service
  - build/jaco.socket
  - build/release.sh
  - build/install.sh.tpl
  - build/uninstall.sh
---

# Release and packaging

How JACO releases are built, signed, and published. The pipeline runs
in [`.github/workflows/release.yml`](../../.github/workflows/release.yml);
the packaging recipe is [`nfpm.yaml`](../../nfpm.yaml); the local
preview path is `make package` / `make release`.

## Artifact set per release

For each tag `vX.Y.Z`, the pipeline produces, per `linux/<arch>` in
`{amd64, arm64}`:

- `jaco_<X.Y.Z>_<arch>.deb`
- `jaco-<X.Y.Z>-1.<arch>.rpm`
- `jaco_<X.Y.Z>_<arch>.apk`
- `jaco-v<X.Y.Z>-linux-<arch>.tar.gz`

Plus a single `SHA256SUMS` listing every artifact, and (once signing
is wired) `SHA256SUMS.minisig`.

The current pipeline runs unsigned; the verification surface in
`jaco self-upgrade` already requires the minisig, so signing is the
gating step before self-upgrade is operationally complete. See
"Verification key rotation" below.

## What's inside each artifact

### `.deb` / `.rpm` / `.apk`

nfpm-built from [`nfpm.yaml`](../../nfpm.yaml). Lays down:

- `/usr/local/bin/jaco` and `/usr/local/bin/jacod` (mode 0755).
- `/lib/systemd/system/jaco.service`.
- `/lib/systemd/system/jaco.socket` — local-control socket unit, pulled
  in by `jaco.service` via `Requires=` so systemd binds the socket in
  the host namespace (issue #167).
- `/etc/jaco/jacod.yaml` (`type: config|noreplace` — operator edits
  survive upgrade).

Postinstall, preremove, and postremove hooks under
`build/packaging/` handle daemon-reload, clean removal, and preserving
the operator's systemd state across upgrades. The hooks inspect the
maintainer-script argument so they distinguish an **upgrade** from a
**removal** (deb `remove` / rpm `0` = removal; deb `upgrade <ver>` /
rpm `1` = upgrade):

- **preremove** only stops + disables `jaco` on a real removal. On an
  upgrade it does nothing, so an already-enabled+running node keeps its
  state (issue #173).
- **postinstall** restarts `jaco` after an upgrade **only if** the unit
  was already enabled and active, so the new binary is picked up. A
  fresh install still never auto-enables or auto-starts — the operator
  must `systemctl enable --now jaco` after editing the config.

Per-format dependencies:

- `.deb` → `docker.io | docker-ce | docker-engine`
- `.rpm` → `/usr/bin/docker`
- `.apk` → `docker`

Alpine uses OpenRC; the systemd unit ships in the package but the
postinstall reload is gated on `systemctl` presence so the install
succeeds on musl systems even though the unit sits unused.

### Generic tarball

Built by the release workflow (`linux/<arch>`) and by the local
`build/release.sh` (linux + darwin, both archs). Contains:

- `jaco`, `jacod` (static, `CGO_ENABLED=0`, `-trimpath -ldflags
  "-s -w -X main.version=<tag>"`).
- `jaco.service`, `jaco.socket`, `jacod.yaml`, `install.sh`,
  `uninstall.sh`, `LICENSE`, `README.md`.

The tarball's `install.sh` lays down both units, the binaries, and the
config; `uninstall.sh` is its symmetric counterpart.

## Local preview

```sh
# build .deb/.rpm/.apk locally — defaults to amd64, version from
# `git describe`. Override:
make package
make package PACKAGE_ARCH=arm64 PACKAGE_VERSION=0.1.0

# cross-build linux + darwin × amd64 + arm64 tarballs into dist/.
# Set MINISIGN_KEY=... to also sign dist/SHA256SUMS.
make release
```

`make package` requires `nfpm` on PATH (install with
`go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.46.3`).

## Cutting a release

1. Bump version where needed in code (none today — version is baked
   in via `-ldflags "-X main.version=<tag>"`).
2. Update `CHANGELOG.md` (if/when one exists).
3. Confirm `make ci-test vet lint` is clean.
4. Tag and push: `git tag v0.2.0 && git push --tags`.
5. The `release` workflow triggers on tags matching `v*`. It builds
   binaries, packages, and the tarball per arch; assembles
   `SHA256SUMS`; uploads everything to a **draft** GitHub release for
   the tag.
6. Once signing is wired, the workflow also produces
   `SHA256SUMS.minisig`.
7. Publish the draft.

## Signing (planned)

`jaco self-upgrade` verifies releases by:

1. minisign signature over `SHA256SUMS` against the embedded public
   key (`internal/packaging/release-pubkey.txt`).
2. SHA-256 of the tarball against the corresponding line in
   `SHA256SUMS`.

Both verifications must pass before either binary is touched on disk.
Any mismatch surfaces as `Error{code: upgrade_verification_failed}`
and the upgrade aborts.

The release workflow currently produces artifacts unsigned; a
GPG-signing job is included commented-out, awaiting the project's
minisign signing key being uploaded to the `MINISIGN_SIGNING_KEY`
secret. Until signing is live, `jaco self-upgrade` will refuse to
proceed against the unsigned releases — provision new clusters from
the packages or generic tarball, not via self-upgrade.

### Key rotation

If the signing key needs to rotate (compromise, schedule):

1. Generate a new minisign keypair offline.
2. Publish a new JACO release whose
   `internal/packaging/release-pubkey.txt` is the **new** key, signed
   with the **old** key (operators are running old code that verifies
   against the old key).
3. Operators upgrade to that release; their daemons now verify future
   releases with the new key.
4. The next release after that is signed with the new key.

## What goes where between versions

- gRPC field additions are backward-compatible. Operators may run an
  N+1 CLI against an N daemon within the same major version.
- Raft FSM apply must remain compatible across adjacent versions. New
  command variants land additively under the `Command{}` proto's
  `oneof`.
- `jacod.yaml` schema is closed — adding a key requires a
  loader update plus a defaulting rule so existing configs continue to
  parse.
- Audit-event types are append-only on `AuditEventType`. Renames
  break backward compatibility for audit consumers.

## See also

- [`jaco self-upgrade`](../cli/self-upgrade.md)
- [Upgrades](../operations/upgrades.md)
- [Repository layout](repo-layout.md)
