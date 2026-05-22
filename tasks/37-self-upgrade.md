Parent slice: [packaging](../slices/packaging.md)
Depends on: 35, 36

# Task 37 — self-upgrade

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Embedded minisign public key + `VerifyTarball` + `jaco self-upgrade` subcommand with atomic binary swap and previous-binary rollback on failure.

## Tasks
- [ ] Create `packaging/release-pubkey.txt` (placeholder content for development; release CI replaces it with the real minisign pubkey before each tagged build).
- [ ] Create `internal/packaging/embed.go` containing `//go:embed release-pubkey.txt` mapped to `var EmbeddedPubKey string`.
- [ ] Create `internal/packaging/verify.go` with `VerifyTarball(tarballPath, checksumsPath, signaturePath string, pubkey string) error`. Uses `github.com/jedisct1/go-minisign` (or equivalent vetted minisign Go lib): verify the `.minisig` signature against the `SHA256SUMS` file contents; then SHA-256 the tarball and grep the matching `<hash>  <filename>` line in `SHA256SUMS`; mismatch on either step → `Error{code:"upgrade_verification_failed", details:{step}}`.
- [ ] Create `cmd/jaco/self_upgrade.go` registering `jaco self-upgrade --url <https://…/jaco-vX.Y.Z-<os>-<arch>.tar.gz>`. Workflow:
  1. Derive `<url>.sha256sums` and `<url>.sha256sums.minisig` URL siblings; fetch all three.
  2. Call `VerifyTarball`; abort on failure.
  3. Extract tarball to a temp dir.
  4. Save current `/usr/local/bin/jaco` to `${JACO_DATA_DIR}/jaco.prev`.
  5. Atomic `os.Rename(tmp/jaco, /usr/local/bin/jaco)`.
  6. `systemctl restart jaco`.
  7. Poll `Cluster.Status` against `localhost:7000` every 2s, up to 60s. Success: this node back in the member list as `follower` or `leader`. Failure: restore `jaco.prev` (atomic rename back), `systemctl restart jaco`, return `Error{code:"upgrade_failed", details:{last_status}}`.
- [ ] Daemon-side: on successful poll, raft-Apply `Command{AuditAppend}{type: UPGRADE_SUCCEEDED, identity, payload:{from, to}}`. On rollback path, raft-Apply `Command{AuditAppend}{type: UPGRADE_FAILED, ...}` from the next restart.
- [ ] Unit tests in `internal/packaging/verify_test.go` using fixtures `testdata/packaging/`: valid tarball + checksums + signature → passes; flip one byte in tarball → fails with `upgrade_verification_failed{step:"checksum"}`; bad signature → fails with `step:"signature"`.
- [ ] E2E `scripts/test/self-upgrade.sh` in a VM: upgrade from a local `file://` URL of `vTest1` → `vTest2` succeeds; corrupted tarball aborts cleanly without touching the binary in `/usr/local/bin/jaco`.

## Acceptance criteria
- [ ] `go test ./internal/packaging/... -race -count=1` exits 0.
- [ ] `bash scripts/test/self-upgrade.sh` exits 0 in a VM.
- [ ] Test asserts a corrupted tarball does NOT modify `/usr/local/bin/jaco` (mtime unchanged) and produces `upgrade_verification_failed`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
