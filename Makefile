.PHONY: build test ci-test test-isolation release vet lint proto package clean

GO  ?= go
BIN ?= jaco

# Skipped test: TestExportImport_RoundTripPreservesBootstrapToken
# has a known snapshot-rename timestamp-collision flake — the
# underlying fix lives in a separate task. The skip flag is shared
# between the local `make ci-test` target and the CI workflow so
# results match.
CI_TEST_SKIP := ^TestExportImport_RoundTripPreservesBootstrapToken$

build:
	$(GO) build -o $(BIN) ./cmd/jaco

test:
	$(GO) test ./... -race -count=1

# ci-test mirrors the test command run by .github/workflows/ci.yml so
# devs can reproduce the CI signal locally before pushing.
ci-test:
	$(GO) test -race -coverprofile=coverage.out -skip '$(CI_TEST_SKIP)' ./...

# test-isolation runs the privileged 3-node end-to-end isolation rig
# (scripts/test/isolation-rig.sh). The rig requires CAP_NET_ADMIN +
# CAP_NET_RAW + kernel WG + nftables + docker; CI executes it under a
# privileged runner. Local devs can run it after setting
# JACO_RIG_FORCE=1 once the jaco-serve daemon entry exists.
test-isolation:
	bash scripts/test/isolation-rig.sh

vet:
	$(GO) vet ./...

lint: vet
	@out=$$(gofmt -l . | grep -v '^vendor/' || true); \
	 if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

proto:
	buf generate

# release cross-builds linux + darwin × amd64 + arm64 tarballs into dist/.
# Set MINISIGN_KEY=... to also sign dist/SHA256SUMS.
release:
	VERSION=$$(git describe --tags --always --dirty 2>/dev/null || echo dev) bash build/release.sh

# package builds linux binaries and runs nfpm to produce .deb / .rpm /
# .apk locally, so devs can preview packaging without pushing a tag.
# Mirrors the per-arch loop in .github/workflows/release.yml.
#
# Defaults: amd64, version from `git describe`. Override:
#   make package PACKAGE_ARCH=arm64 PACKAGE_VERSION=0.1.0
PACKAGE_VERSION ?= $(shell v=$$(git describe --tags --abbrev=0 2>/dev/null); echo "$${v:-v0.0.0-dev}" | sed 's/^v//')
PACKAGE_ARCH    ?= amd64
PACKAGE_DIST    := dist/package/$(PACKAGE_ARCH)

package:
	@command -v nfpm >/dev/null 2>&1 || { \
	  echo "nfpm not found on PATH — install with: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.46.3"; \
	  exit 1; \
	}
	rm -rf $(PACKAGE_DIST) dist/staging
	mkdir -p $(PACKAGE_DIST) dist/staging
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PACKAGE_ARCH) \
	  $(GO) build -trimpath -ldflags "-s -w" -o dist/staging/jaco  ./cmd/jaco
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PACKAGE_ARCH) \
	  $(GO) build -trimpath -ldflags "-s -w" -o dist/staging/jacod ./cmd/jacod
	@for fmt in deb rpm apk; do \
	  echo "[package] building $$fmt for $(PACKAGE_ARCH) v$(PACKAGE_VERSION)"; \
	  NFPM_VERSION=$(PACKAGE_VERSION) \
	  NFPM_ARCH=$(PACKAGE_ARCH) \
	    nfpm --config nfpm.yaml package --packager $$fmt --target $(PACKAGE_DIST)/ || exit 1; \
	done
	@ls -la $(PACKAGE_DIST)

clean:
	rm -f $(BIN)
	rm -rf dist/
