.PHONY: build build-release vet test test-acceptance

# Build a development binary in the repo root.
build:
	CGO_ENABLED=0 go build -o engram ./cmd/engram

# Build a stripped release binary (smaller, no debug info).
build-release:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o engram ./cmd/engram

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
