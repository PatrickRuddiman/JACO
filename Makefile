.PHONY: build test test-isolation vet lint proto clean

GO  ?= go
BIN ?= jaco

build:
	$(GO) build -o $(BIN) ./cmd/jaco

test:
	$(GO) test ./... -race -count=1

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

clean:
	rm -f $(BIN)
	rm -rf dist/
