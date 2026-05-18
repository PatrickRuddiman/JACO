Parent spec: [spec.md](../spec.md) · Design: [design.md](../design.md)

# JACO — packaging

## §1 Summary

Release pipeline, install/uninstall flow, and `jaco self-upgrade` mechanics. Produces one tarball per (os, arch) and signs it; operators install via `./install.sh` (unattended with env overrides); daemons replace themselves in-place via `jaco self-upgrade <url>` with signature + checksum verification.

## §2 Codebase reconnaissance

Greenfield: no existing system to reconcile. Decisions below are unconstrained.

## §3 Decisions

1. **Release artifact format.** Options: tar.gz bundle, standalone binary + remote script, both. **Chosen:** single tar.gz per (os, arch) containing binary, `install.sh`, systemd unit file, LICENSE. Rationale: matches terraform/consul/nomad muscle memory; one URL to download; easy to mirror.
2. **Install script.** Options: unattended-with-env-overrides, interactive, two scripts. **Chosen:** fully unattended `install.sh` with env overrides (`JACO_PREFIX`, `JACO_DATA_DIR`, `JACO_USER`). Rationale: scripts and humans both work; idempotent reruns are safe; no prompts in CI.
3. **Self-upgrade verification.** Options: SHA256 + minisign, SHA256 only, GPG. **Chosen:** SHA256 manifest + minisign signature, both required. Rationale: minisign is a 16-line spec with one Ed25519 key; far smaller surface than GPG; meaningfully better than checksum-only against registry compromise.
4. **Data directory location and ownership.** Options: `/var/lib/jaco` system path, per-user home, fully configurable. **Chosen:** `/var/lib/jaco` owned by `jaco:jaco`, mode `0700`, created by `install.sh`. Rationale: standard FHS; daemon needs CAP_NET_ADMIN and runs as a system service.

## §4 Contracts & shapes

Module layout under `internal/packaging/` and root build scripts:

- `cmd/jaco/self_upgrade.go` — implements `jaco self-upgrade <url>`. Downloads the tarball, checksums file, and signature file; verifies; extracts; replaces `/usr/local/bin/jaco`; runs `systemctl restart jaco`; waits for raft rejoin.
- `internal/packaging/verify.go` — `VerifyTarball(tarball, checksums, signature, pubkey) error`. Pubkey is compiled in as a Go `var EmbeddedPubKey = `…`` constant; rotation requires a new release with the new key.
- `internal/packaging/embed.go` — embeds the minisign public key (`packaging/release-pubkey.txt`) via `//go:embed`.
- `build/release.sh` — CI script that builds the binary, generates `SHA256SUMS`, signs with `minisign -S`, assembles the tarball, uploads to GitHub releases.
- `build/install.sh.tpl` — template for the install script; baked into the tarball during build (versioned).
- `build/jaco.service` — systemd unit file source.

Release tarball contents (`jaco-<version>-<os>-<arch>.tar.gz`):

- `jaco` — the static binary
- `install.sh` — install script (executable)
- `jaco.service` — systemd unit
- `LICENSE` — MPL-2.0 or BUSL or whatever JACO ships under
- `README.md` — minimal "run `./install.sh`" instructions

Adjacent files on the release page (not in the tarball):

- `SHA256SUMS` — one line per artifact, `<hex>  <filename>`
- `SHA256SUMS.minisig` — minisign signature of the SHA256SUMS file

Install script behavior (`./install.sh`):

- Detects OS/arch; refuses to proceed if the tarball was for a different platform.
- Creates `jaco` user (uid auto) and `jaco` group if not present, no login shell.
- Creates `/var/lib/jaco` mode `0700` owned by `jaco:jaco` (or `$JACO_DATA_DIR` if set).
- Copies `jaco` to `$JACO_PREFIX/bin/jaco` (default `/usr/local`); chmod 0755.
- Installs systemd unit at `/etc/systemd/system/jaco.service`; `systemctl daemon-reload`; `systemctl enable jaco` (does NOT start — operator runs `jaco bootstrap` or `jaco node join` first, which writes initial state in `/var/lib/jaco`).
- Prints next-step instructions to stdout.
- Idempotent: re-running upgrades the binary in-place if version differs; warns and exits 0 if same version already installed.

systemd unit shape (`/etc/systemd/system/jaco.service`):

- `Type=notify` (the daemon sends `sd_notify(READY=1)` once raft and ingress are up).
- `ExecStart=/usr/local/bin/jaco serve` (no other args; daemon reads config from env + `/var/lib/jaco`).
- `User=jaco Group=jaco`.
- `AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW` (for WG, ports 80/443, raw socket if needed for nftables).
- `Restart=on-failure RestartSec=5`.
- `Environment=JACO_DATA_DIR=/var/lib/jaco`
- `LimitNOFILE=65536`.

Self-upgrade verification flow:

1. CLI fetches `<url>`, `<url>.sha256sums`, `<url>.sha256sums.minisig` (or all three from a single base URL pattern).
2. `verify.VerifyTarball`:
   - Loads embedded pubkey.
   - Runs minisign verify on the `.minisig` file against the `SHA256SUMS` content.
   - Computes SHA256 of the downloaded tarball; compares to the matching line in `SHA256SUMS`.
3. On any verification failure: aborts before touching `/usr/local/bin/jaco`, exits non-zero with `Error{code: upgrade_verification_failed, details: {step}}`.
4. On success: extracts tarball to a temp dir, atomically renames `tmp/jaco` over `/usr/local/bin/jaco`.

Self-upgrade restart flow:

1. After binary swap, CLI calls `systemctl restart jaco` (the running daemon process exits; systemd starts the new one).
2. CLI polls `jaco status --server localhost:7000` every 2s for up to 60s.
3. Success: status returns and reports this node back in the member list as `follower` or `leader`.
4. Failure: rolls back by extracting the prior binary (saved at `/var/lib/jaco/jaco.prev` before swap) and restarting; reports `Error{code: upgrade_failed, details: {last_status}}`.

Uninstall script (`uninstall.sh`, shipped in the same tarball):

- `systemctl stop jaco && systemctl disable jaco`.
- Removes `/etc/systemd/system/jaco.service`, `systemctl daemon-reload`.
- Removes `/usr/local/bin/jaco`.
- Removes `/var/lib/jaco` unless `--preserve-data` flag.
- Removes `jaco` user/group (only if `/var/lib/jaco` was removed).

## §5 Sequence

CI release pipeline (per tagged version):

1. CI builds the binary for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`.
2. For each (os, arch): copies binary + `install.sh.tpl` (rendered with version) + `jaco.service` + LICENSE into `jaco-<version>-<os>-<arch>/`; tars + gzips.
3. After all four tarballs exist: writes `SHA256SUMS` listing all four; runs `minisign -S -s release.key -m SHA256SUMS` producing `SHA256SUMS.minisig`.
4. Uploads tarballs + checksums + signature to GitHub release for the tag.

Operator fresh install:

1. `curl -LO https://github.com/.../jaco-1.0.0-linux-amd64.tar.gz`
2. `tar -xzf jaco-1.0.0-linux-amd64.tar.gz && cd jaco-1.0.0-linux-amd64`
3. `sudo ./install.sh`
4. `sudo /usr/local/bin/jaco bootstrap --name $(hostname)` (first node) OR `sudo /usr/local/bin/jaco node join …` (subsequent).
5. `sudo systemctl start jaco` (started by `jaco bootstrap`/`node join` automatically? — yes, those commands invoke `systemctl start` after writing initial state, so the operator typically never types this).

Operator rolling upgrade:

1. Operator picks one node at a time.
2. `sudo jaco self-upgrade --url https://github.com/.../jaco-1.1.0-linux-amd64.tar.gz`
3. CLI verifies signature + checksum; swaps binary; restarts daemon; waits for raft rejoin.
4. Operator runs `jaco status` from any node to confirm node is back as follower/leader.
5. Repeat on the next node.

Verification key rotation (rare, security incident):

1. Generate new minisign keypair offline.
2. Embed new pubkey into a new JACO release (still signed with the OLD key, since operators are running OLD code).
3. Operators upgrade to that release; their daemon now verifies future releases with the new key.
4. Next release after that is signed with the new key.

## §6 Out of scope

- Container image distribution of JACO itself (design §7 Out: no docker-in-docker for the daemon in v1).
- Debian/RPM packages (design §3 chose static binary + tarball; packages defer to v2).
- Windows / FreeBSD / illumos builds (spec §4 Compatibility: Linux daemon, Linux+macOS CLI).
- Auto-update on a timer (operator-triggered upgrades only).
- Cluster-wide coordinated upgrade (`jaco cluster upgrade --all`) — operator drives one node at a time in v1.
- Mirror infrastructure (private apt/dnf repos, S3 mirror automation).

> If the parent spec is ambiguous on anything this slice depends on, stop and update the spec. Do not invent behavior here.
