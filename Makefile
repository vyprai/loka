.PHONY: all build build-linux build-all proto clean test test-unit test-integration lint fmt help \
       fetch-firecracker build-rootfs setup-lima e2e-test

# Variables
GO := go
GOFLAGS := -trimpath
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-s -w -X github.com/vyprai/loka/pkg/version.Version=$(VERSION) -X github.com/vyprai/loka/pkg/version.Commit=$(COMMIT) -X github.com/vyprai/loka/pkg/version.BuildTime=$(BUILD_TIME)"

# Binaries
BIN_DIR := bin
LOKAD := $(BIN_DIR)/lokad
LOKA_WORKER := $(BIN_DIR)/loka-worker
LOKA_SUPERVISOR := $(BIN_DIR)/loka-supervisor
LOKA_CLI := $(BIN_DIR)/loka

# Default target
all: build

# Build all binaries for current platform
build: $(LOKAD) $(LOKA_WORKER) $(LOKA_SUPERVISOR) $(LOKA_CLI)

$(LOKAD):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/lokad

$(LOKA_WORKER):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-worker

$(LOKA_SUPERVISOR):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-supervisor

$(LOKA_CLI):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka

# Build for Linux (cross-compile from macOS)
build-linux:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/lokad ./cmd/lokad
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-worker ./cmd/loka-worker
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-supervisor ./cmd/loka-supervisor
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka ./cmd/loka

# Build all platforms
build-all: build build-linux

# Generate protobuf code
proto:
	@echo "Generating protobuf code..."
	protoc --proto_path=api/proto \
		--go_out=api/lokav1 --go_opt=paths=source_relative \
		--go-grpc_out=api/lokav1 --go-grpc_opt=paths=source_relative \
		types.proto control.proto worker.proto
	protoc --proto_path=api/proto \
		--go_out=api/supervisorv1 --go_opt=paths=source_relative \
		--go-grpc_out=api/supervisorv1 --go-grpc_opt=paths=source_relative \
		supervisor.proto

# Run all tests
test: test-unit

# Unit tests (macOS-safe, no KVM required)
test-unit:
	$(GO) test -v -race -count=1 -tags=unit ./...

# Integration tests (Linux + KVM required)
test-integration:
	$(GO) test -v -race -count=1 -tags=integration ./...

# Firecracker setup (Linux required)
fetch-firecracker:
	bash scripts/fetch-firecracker.sh

# Build guest rootfs image (Linux + Docker required)
build-rootfs: build-linux
	bash scripts/build-rootfs.sh

# Setup Lima VM for macOS development
setup-lima:
	bash scripts/setup-lima.sh

# Run E2E test suite
e2e-test: build
	bash scripts/e2e-test.sh

# Lint
lint:
	golangci-lint run ./...

# Format
fmt:
	$(GO) fmt ./...
	goimports -w .

# Clean build artifacts
clean:
	rm -rf $(BIN_DIR)

# Help
help:
	@echo "LOKA - Session-Based MicroVM Execution OS for AI Agents"
	@echo ""
	@echo "Targets:"
	@echo "  build            Build all binaries for current platform"
	@echo "  build-linux      Cross-compile all binaries for Linux amd64"
	@echo "  build-all        Build for all platforms"
	@echo "  proto            Generate protobuf code"
	@echo "  test             Run all tests (unit)"
	@echo "  test-unit        Run unit tests (macOS-safe)"
	@echo "  test-integration Run integration tests (Linux + KVM)"
	@echo "  lint             Run linter"
	@echo "  fmt              Format code"
	@echo "  clean            Remove build artifacts"
	@echo "  help             Show this help"
