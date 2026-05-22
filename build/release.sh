#!/usr/bin/env bash
# build/release.sh — cross-build + tarball + checksum + optional sign.
#
# Required:  VERSION (release tag, e.g. v0.1.0 or test).
# Optional:  MINISIGN_KEY (path to minisign private key — skips signing when
#            unset and prints a warning).
#
# Targets: linux + darwin × amd64 + arm64 — 4 tarballs in ./dist/.
#
# Acceptance criteria checked by `make release` + the per-task ACs:
#   - `VERSION=test bash build/release.sh` exits 0.
#   - All four tarballs land in dist/.
#   - dist/SHA256SUMS lists exactly 4 entries.
#   - Each tarball contains: <name>/, jaco, install.sh, jaco.service,
#     LICENSE, README.md.

set -euo pipefail

: "${VERSION:?VERSION env var is required (e.g. VERSION=v0.1.0)}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$REPO_ROOT/dist"
BUILD_DIR="$REPO_ROOT/build"

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

build_one() {
  local os="$1" arch="$2"
  local stage_name="jaco-${VERSION}-${os}-${arch}"
  local stage_dir="$DIST_DIR/$stage_name"
  mkdir -p "$stage_dir"

  echo "[release] building $os/$arch -> $stage_dir/jaco" >&2
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o "$stage_dir/jaco" \
      "$REPO_ROOT/cmd/jaco"

  # Render install.sh from the template by substituting __VERSION__.
  sed "s/__VERSION__/${VERSION}/g" "$BUILD_DIR/install.sh.tpl" > "$stage_dir/install.sh"
  chmod 0755 "$stage_dir/install.sh"

  cp "$BUILD_DIR/jaco.service" "$stage_dir/jaco.service"
  cp "$REPO_ROOT/LICENSE"      "$stage_dir/LICENSE"
  cp "$REPO_ROOT/README.md"    "$stage_dir/README.md"

  # Tar+gz (deterministic ordering for reproducibility).
  tar -C "$DIST_DIR" \
      --sort=name \
      --owner=0 --group=0 --numeric-owner \
      --mtime='UTC 2026-01-01' \
      -czf "$DIST_DIR/${stage_name}.tar.gz" \
      "$stage_name"

  rm -rf "$stage_dir"
}

for os in linux darwin; do
  for arch in amd64 arm64; do
    build_one "$os" "$arch"
  done
done

# SHA256SUMS over the 4 tarballs.
(cd "$DIST_DIR" && sha256sum *.tar.gz > SHA256SUMS)
echo "[release] wrote $DIST_DIR/SHA256SUMS"

# Optional signing.
if [[ -n "${MINISIGN_KEY:-}" ]]; then
  if ! command -v minisign >/dev/null 2>&1; then
    echo "[release] minisign not on PATH — skipping signing" >&2
  else
    echo "[release] signing SHA256SUMS with $MINISIGN_KEY" >&2
    minisign -S -s "$MINISIGN_KEY" -m "$DIST_DIR/SHA256SUMS"
  fi
else
  echo "[release] MINISIGN_KEY unset — skipping signing" >&2
fi

echo "[release] done"
ls -la "$DIST_DIR" >&2
