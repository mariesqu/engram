.PHONY: build build-release vet test test-acceptance release

# Binary name: engram.exe on Windows, engram elsewhere.
BIN := engram$(if $(filter Windows_NT,$(OS)),.exe)

# VERSION is injected at link time.  Default: git describe (includes tag,
# distance, and short SHA — e.g. v0.1.0 on a clean tag, v0.1.0-3-gabcd123
# when 3 commits ahead).  Falls back to "dev" when no tags exist yet.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

# LDFLAGS stamps the version and strips debug info for release builds.
RELEASE_LDFLAGS := -s -w -X 'main.version=$(VERSION)'

# Build a development binary in the repo root (no version stamping).
build:
	CGO_ENABLED=0 go build -o $(BIN) ./cmd/engram

# Build a stripped release binary with version stamped via ldflags.
build-release:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(BIN) ./cmd/engram

# Run go vet for both the default and acceptance build tags.
vet:
	go vet ./...
	go vet -tags acceptance ./...

# Run unit tests (no external services required).
test:
	go test ./... -count=1

# Run the full acceptance suite (uses embedded-postgres; no Docker needed).
test-acceptance:
	go test -tags acceptance ./... -count=1 -timeout 120s

# Cross-build release binaries for the distribution matrix into dist/.
# Produces one binary per GOOS/GOARCH pair plus a SHA256SUMS file.
# Usage:
#   make release                        # uses git-describe VERSION
#   make release VERSION=v0.2.0        # explicit tag
release:
	@mkdir -p dist
	@echo "Building engram $(VERSION) for release matrix..."
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o dist/engram-$(VERSION)-linux-amd64    ./cmd/engram
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o dist/engram-$(VERSION)-linux-arm64    ./cmd/engram
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o dist/engram-$(VERSION)-darwin-amd64   ./cmd/engram
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o dist/engram-$(VERSION)-darwin-arm64   ./cmd/engram
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o dist/engram-$(VERSION)-windows-amd64.exe ./cmd/engram
	@echo "Generating SHA256SUMS..."
	@cd dist && sha256sum engram-$(VERSION)-* > SHA256SUMS
	@echo "Release artifacts in dist/:"
	@ls -lh dist/
