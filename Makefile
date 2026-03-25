.PHONY: all build build-linux build-all proto clean test test-unit test-integration lint fmt help \
       install uninstall fetch-firecracker build-vm-assets lokavm-full install-vm-assets e2e-test lokavm

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
LOKA_VMAGENT := $(BIN_DIR)/loka-vmagent
LOKA_CLI := $(BIN_DIR)/loka
LOKA_VM := $(BIN_DIR)/lokavm

# Default target
all: build

# Build all binaries for current platform
# lokavm is macOS-only (VZ framework); included conditionally.
build: $(LOKAD) $(LOKA_WORKER) $(LOKA_SUPERVISOR) $(LOKA_VMAGENT) $(LOKA_CLI)
ifeq ($(shell uname -s),Darwin)
build: $(LOKA_VM)
endif

$(LOKAD):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/lokad

$(LOKA_WORKER):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-worker

$(LOKA_SUPERVISOR):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-supervisor

$(LOKA_VMAGENT):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-vmagent

$(LOKA_CLI):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka

$(LOKA_VM):
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/lokavm
	@echo "  Signing with VZ entitlement..."
	@printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>' > /tmp/lokavm.entitlements
	@codesign --entitlements /tmp/lokavm.entitlements --force -s - $@ 2>/dev/null || true
	@rm -f /tmp/lokavm.entitlements

# Standalone lokavm target (macOS only, requires CGO for VZ framework)
lokavm: $(LOKA_VM)

# Build for Linux (cross-compile from macOS)
build-linux:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/lokad ./cmd/lokad
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-worker ./cmd/loka-worker
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-supervisor ./cmd/loka-supervisor
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-vmagent ./cmd/loka-vmagent
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka ./cmd/loka
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/lokad ./cmd/lokad
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka-worker ./cmd/loka-worker
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka-supervisor ./cmd/loka-supervisor
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka-vmagent ./cmd/loka-vmagent
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka ./cmd/loka

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

# Build VM rootfs + kernel (no Docker — just Alpine minirootfs + sudo mount)
build-vm-assets:
	bash scripts/build-rootfs.sh build/loka-rootfs.ext4

# Build everything for lokavm (macOS): CLI + lokavm + VM assets
lokavm-full: $(LOKA_CLI) $(LOKA_VM) build-vm-assets
	@echo ""
	@echo "  lokavm ready!"
	@echo "  Run: ./bin/lokavm --data-dir build/"

# Install VM assets to ~/.loka/vm/
install-vm-assets: build-vm-assets
	@mkdir -p $(HOME)/.loka/vm
	@cp build/vmlinux $(HOME)/.loka/vm/vmlinux
	@cp build/loka-rootfs.ext4 $(HOME)/.loka/vm/rootfs.ext4
	@echo "  VM assets installed to ~/.loka/vm/"

# Install LOKA locally from source
INSTALL_DIR ?= /usr/local/bin
install: build
ifeq ($(shell uname -s),Darwin)
install: install-vm-assets
	@echo "==> Installing LOKA (macOS)"
	sudo install -m 755 $(LOKA_CLI) $(INSTALL_DIR)/loka
	sudo install -m 755 $(LOKA_VM) $(INSTALL_DIR)/lokavm
	@echo ""
	@echo "  LOKA installed. Run: loka setup local"
else
	sudo install -m 755 $(LOKA_CLI) $(INSTALL_DIR)/loka
	sudo install -m 755 $(LOKAD) $(INSTALL_DIR)/lokad
	sudo install -m 755 $(LOKA_WORKER) $(INSTALL_DIR)/loka-worker
	sudo install -m 755 $(LOKA_SUPERVISOR) $(INSTALL_DIR)/loka-supervisor
	@echo "  LOKA installed. Run: lokad --config /etc/loka/controlplane.yaml"
endif

# Uninstall LOKA
uninstall:
	@echo "==> Uninstalling LOKA"
ifeq ($(shell uname -s),Darwin)
	-loka setup down 2>/dev/null || true
	-pkill -f lokavm 2>/dev/null || true
endif
	-sudo rm -f $(INSTALL_DIR)/loka $(INSTALL_DIR)/lokad $(INSTALL_DIR)/loka-worker $(INSTALL_DIR)/loka-supervisor $(INSTALL_DIR)/lokavm
	-sudo rm -f $(INSTALL_DIR)/firecracker
	-rm -rf $(HOME)/.loka
	@echo "  ✓ LOKA uninstalled"


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
	@echo "  lokavm           Build lokavm binary (macOS only, VZ framework)"
	@echo "  lokavm-full      Build lokavm + VM assets (kernel + rootfs)"
	@echo "  build-vm-assets  Build VM rootfs + download kernel (no Docker)"
	@echo "  install          Build + install locally (lokavm + assets on macOS)"
	@echo "  uninstall        Remove LOKA and all data"
	@echo "  build-linux      Cross-compile all binaries for Linux"
	@echo "  build-all        Build for all platforms"
	@echo "  e2e-test         Run E2E test suite"
	@echo "  proto            Generate protobuf code"
	@echo "  test             Run all tests (unit)"
	@echo "  lint             Run linter"
	@echo "  fmt              Format code"
	@echo "  clean            Remove build artifacts"
	@echo "  help             Show this help"
