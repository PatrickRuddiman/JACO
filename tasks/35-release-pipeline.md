Parent slice: [packaging](../slices/packaging.md)
Depends on: 00

# Task 35 — release-pipeline

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`build/release.sh` cross-builds linux + darwin × amd64 + arm64 binaries, assembles four tarballs, generates `SHA256SUMS`, and signs with minisign.

## Tasks
- [ ] Create `build/release.sh` (executable). Reads `VERSION` env (required); `MINISIGN_KEY` env (optional — when absent, skip signing step and warn).
- [ ] For each `(os, arch)` in `{linux,darwin}×{amd64,arm64}`: `CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags "-s -w -X main.version=$VERSION" -o dist/jaco-$VERSION-$os-$arch/jaco ./cmd/jaco`.
- [ ] Copy into each staging dir: rendered `install.sh` (from `build/install.sh.tpl` with `__VERSION__` placeholder replaced), `jaco.service` (from `build/jaco.service`), `LICENSE`, `README.md`.
- [ ] Tar+gz: `tar -C dist -czf dist/jaco-$VERSION-$os-$arch.tar.gz jaco-$VERSION-$os-$arch/`.
- [ ] After all 4 tarballs exist: `(cd dist && sha256sum *.tar.gz > SHA256SUMS)`.
- [ ] If `MINISIGN_KEY` is set: `minisign -S -s "$MINISIGN_KEY" -m dist/SHA256SUMS` → produces `dist/SHA256SUMS.minisig`.
- [ ] Create `build/install.sh.tpl` (placeholder `__VERSION__`; full body comes in task 36).
- [ ] Create `build/jaco.service` (full body comes in task 36).
- [ ] Wire `make release` Makefile target to `VERSION=$(shell git describe --tags) build/release.sh`.

## Acceptance criteria
- [ ] `VERSION=test bash build/release.sh` exits 0.
- [ ] `for o in linux darwin; do for a in amd64 arm64; do test -f dist/jaco-test-$o-$a.tar.gz || exit 1; done; done` exits 0.
- [ ] `tar -tzf dist/jaco-test-linux-amd64.tar.gz | sort` lists exactly `jaco-test-linux-amd64/`, `jaco-test-linux-amd64/LICENSE`, `jaco-test-linux-amd64/README.md`, `jaco-test-linux-amd64/install.sh`, `jaco-test-linux-amd64/jaco`, `jaco-test-linux-amd64/jaco.service`.
- [ ] `test -f dist/SHA256SUMS && wc -l < dist/SHA256SUMS` prints `4`.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
