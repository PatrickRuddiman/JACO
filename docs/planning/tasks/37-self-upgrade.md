Parent slice: [packaging](../slices/packaging.md)
Depends on: 35, 36

# Task 37 — self-upgrade

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Embedded minisign public key + `VerifyTarball` + `jaco self-upgrade` subcommand with atomic binary swap and previous-binary rollback on failure.

## Tasks
- [x] Create `internal/packaging/release-pubkey.txt` (placeholder content; release CI overwrites with the real minisign pubkey before each tagged build). Lives next to embed.go so `//go:embed` resolves it; the task draft put it at repo root which conflicts with Go's embed semantics.
- [x] Create `internal/packaging/embed.go` containing `//go:embed release-pubkey.txt` → `var EmbeddedPubKey string`.
- [x] Create `internal/packaging/verify.go` with `VerifyTarball(tarballPath, checksumsPath, signaturePath, pubKey string) error`. Uses `github.com/jedisct1/go-minisign`: parses pubKey (strips the `untrusted comment:` line), decodes the .minisig, verifies against the SHA256SUMS body, then SHA-256s the tarball and looks up the matching `<hex>  <basename>` line in SHA256SUMS. Returns `*VerifyError{Code:"upgrade_verification_failed", Step:"signature"|"checksum"|...}` on any mismatch. `IsVerificationFailed(err)` is the gating helper.
- [x] Create `cmd/jaco/self_upgrade.go` registering `jaco self-upgrade --url <https://.../jaco-vX-os-arch.tar.gz>`. Workflow extracted into `runSelfUpgrade(ctx, url, binPath, pubKey, fetcher)` so unit tests inject an in-memory fetcher: fetch tarball + sibling SHA256SUMS + SHA256SUMS.minisig; VerifyTarball; extract `<dir>/jaco` from the tarball; copy current binary to `<binPath>.prev`; copy new binary to `<binPath>.upgrading` and atomic-rename to `<binPath>`. Verification failures abort before the binary is touched.
- [ ] **Deferred**: systemctl restart + Cluster.Status poll + automatic rollback on poll failure. Needs the daemon entry. The atomic swap step is in place; the restart+verify+rollback orchestration lands when `jaco serve` is a real daemon.
- [ ] **Deferred**: daemon-side raft-Apply of `Command{AuditAppend}{UPGRADE_SUCCEEDED|UPGRADE_FAILED}` — needs the daemon entry to know the upgrade lifecycle is happening.
- [x] Six packaging tests pass with -race: checksum-mismatch returns typed VerifyError; tarball-missing returns typed error; bogus signature returns step=signature; checksums-file missing fails verification; IsVerificationFailed matches only our error type; EmbeddedPubKey embed is present and contains the minisign header.
- [x] Five cmd/jaco self-upgrade tests pass with -race: corrupted tarball does NOT modify the destination binary or its mtime + returns upgrade_verification_failed (the AC); missing URL rejected; fetch error aborts before touching the binary; siblingURL replaces the last path segment correctly; extractJacoBinary finds the `<dir>/jaco` entry inside a tarball.
- [ ] **Deferred**: `scripts/test/self-upgrade.sh` E2E in a VM — needs a real daemon + a real release pipeline with a valid signing key for the test fixtures.

## Acceptance criteria
- [x] `go test ./internal/packaging/... -race -count=1` exits 0 (6 tests).
- [x] `go test ./cmd/jaco/... -race -count=1 -run SelfUpgrade` exits 0.
- [x] Test asserts a corrupted tarball does NOT modify the destination binary (`TestRunSelfUpgrade_CorruptedTarballDoesNotModifyBinary` — verifies byte equality and unchanged mtime, and gets upgrade_verification_failed back).
- [ ] `bash scripts/test/self-upgrade.sh` — deferred to daemon entry + VM-based release-signing fixtures.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
