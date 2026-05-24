Parent slice: [packaging](../slices/packaging.md)
Depends on: 00

# Task 35 — release-pipeline

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`build/release.sh` cross-builds linux + darwin × amd64 + arm64 binaries, assembles four tarballs, generates `SHA256SUMS`, and signs with minisign.

## Tasks
- [x] Create `build/release.sh` (executable). Reads `VERSION` env (required) + optional `MINISIGN_KEY` env (skips signing + warns when unset).
- [x] For each `(os, arch)` in `{linux,darwin}×{amd64,arm64}`: cross-builds with `CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o dist/jaco-$VERSION-$os-$arch/jaco ./cmd/jaco`. Deterministic builds via `-trimpath` + `tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='UTC 2026-01-01'`.
- [x] Each staging dir gets: rendered `install.sh` (from `build/install.sh.tpl` with `__VERSION__` substituted via sed), `jaco.service` (from `build/jaco.service`), `LICENSE`, `README.md`.
- [x] Tar+gz the 4 tarballs into `dist/`.
- [x] After all 4 land: `(cd dist && sha256sum *.tar.gz > SHA256SUMS)`. 4-line file.
- [x] If `MINISIGN_KEY` set + `minisign` on PATH: `minisign -S -s $MINISIGN_KEY -m dist/SHA256SUMS` → `dist/SHA256SUMS.minisig`. Unset = warning-only skip.
- [x] Create `build/install.sh.tpl` (placeholder body; full installer with signature-verify + bootstrap-vs-join branches comes in task 36).
- [x] Create `build/jaco.service` (placeholder unit; full body with capability flags + sd_notify wiring comes in task 36).
- [x] Wire `make release` Makefile target. Resolves VERSION from `git describe --tags --always --dirty`, falls back to `dev` when not in a git repo.
- [x] Add minimal `README.md` so the release pipeline has the doc file it ships with the tarball.

## Acceptance criteria
- [x] `VERSION=test bash build/release.sh` exits 0 (verified).
- [x] All four `dist/jaco-test-{linux,darwin}-{amd64,arm64}.tar.gz` exist (verified).
- [x] `tar -tzf dist/jaco-test-linux-amd64.tar.gz` lists exactly the six expected entries: `jaco-test-linux-amd64/`, `.../LICENSE`, `.../README.md`, `.../install.sh`, `.../jaco`, `.../jaco.service` (verified; the AC's exact sort order assumes `LC_ALL=C sort` which differs from default locale-aware sort, but the set is unchanged).
- [x] `wc -l < dist/SHA256SUMS` prints `4` (verified).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
