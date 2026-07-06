# usher — build helpers.
#
# Plain GNU make. No build tool dependencies beyond `go` itself; this matches
# the project's "stdlib-first, minimal toolchain" philosophy.

.PHONY: build test vet check install run clean dist icons lark test-lark help

OUTPUT  := usher
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_ENV  := CGO_ENABLED=0
GO_LD   := -ldflags="-s -w -X main.Version=$(VERSION)"
GO_TAGS := -trimpath

# Default target: build a stripped, statically-linked binary in the repo root.
build:
	$(GO_ENV) go build $(GO_TAGS) $(GO_LD) -o $(OUTPUT) ./cmd/usher

# Run the full test suite.
test:
	go test ./...

# Run `go vet` across all packages.
vet:
	go vet ./...

# Lint + test — what to run before committing.
check: vet test

# Install into $GOBIN (typically ~/go/bin) so `usher` is on PATH everywhere.
install:
	$(GO_ENV) go install $(GO_TAGS) $(GO_LD) ./cmd/usher

# Build then start the server with safe defaults (loopback only).
run: build
	./$(OUTPUT) serve

# Build the Lark IM plugin (separate module: its SDK deps stay out of usher's go.mod).
lark:
	cd plugins/lark && $(GO_ENV) go build $(GO_TAGS) $(GO_LD) -o ../../usher-lark .

# Test the Lark plugin module.
test-lark:
	cd plugins/lark && go test ./... && go vet ./...

# Regenerate icon variants from the source SVG.
icons:
	python3 internal/web/icons-src/gen-icons.py

# Remove build artifacts.
clean:
	rm -rf $(OUTPUT) usher-lark dist

# Cross-compile release binaries for common targets into dist/.
# Each binary is statically linked (CGO off) and has debug info stripped.
dist: clean
	mkdir -p dist
	GOOS=linux  GOARCH=amd64 $(GO_ENV) go build $(GO_TAGS) $(GO_LD) -o dist/$(OUTPUT)-linux-amd64  ./cmd/usher
	GOOS=linux  GOARCH=arm64 $(GO_ENV) go build $(GO_TAGS) $(GO_LD) -o dist/$(OUTPUT)-linux-arm64  ./cmd/usher
	GOOS=darwin GOARCH=amd64 $(GO_ENV) go build $(GO_TAGS) $(GO_LD) -o dist/$(OUTPUT)-darwin-amd64 ./cmd/usher
	GOOS=darwin GOARCH=arm64 $(GO_ENV) go build $(GO_TAGS) $(GO_LD) -o dist/$(OUTPUT)-darwin-arm64 ./cmd/usher
	@echo
	@echo "built:" && ls -lh dist/

help:
	@echo "Common targets:"
	@echo "  build    compile ./$(OUTPUT) (default)"
	@echo "  test     go test ./..."
	@echo "  vet      go vet ./..."
	@echo "  check    vet + test"
	@echo "  run      build + ./$(OUTPUT) serve"
	@echo "  install  go install (into \$$GOBIN)"
	@echo "  dist     cross-compile linux/darwin amd64+arm64 into dist/"
	@echo "  lark     build the Lark IM plugin (./usher-lark)"
	@echo "  icons    regenerate icon PNGs from icons-src/icon-raw.svg"
	@echo "  clean    remove $(OUTPUT) and dist/"
