.PHONY: build test vet lint proto clean

GO  ?= go
BIN ?= jaco

build:
	$(GO) build -o $(BIN) ./cmd/jaco

test:
	$(GO) test ./... -race -count=1

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
