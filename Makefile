.PHONY: all build build-linux build-linux-amd64 build-linux-arm64 build-all proto clean test test-unit test-integration test-metrics test-logging lint fmt help \
       install uninstall install-vm-assets install-cloud-hypervisor e2e-test e2e-test-linux e2e-test-section kernel initramfs kernel-all kernel-update release

# Variables
GO := go
GOFLAGS := -trimpath
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-s -w -X github.com/vyprai/loka/pkg/version.Version=$(VERSION) -X github.com/vyprai/loka/pkg/version.Commit=$(COMMIT) -X github.com/vyprai/loka/pkg/version.BuildTime=$(BUILD_TIME)"

# Platform detection
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

# Suppress macOS linker warning about duplicate -lobjc from Code-Hex/vz CGo.
ifeq ($(UNAME_S),Darwin)
export CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
endif

# Cloud Hypervisor version (Linux VMM)
CH_VERSION ?= v44.0

# Binaries
BIN_DIR := bin
LOKAD := $(BIN_DIR)/lokad
LOKA_WORKER := $(BIN_DIR)/loka-worker
LOKA_SUPERVISOR := $(BIN_DIR)/loka-supervisor
LOKA_VMAGENT := $(BIN_DIR)/loka-vmagent
LOKA_CLI := $(BIN_DIR)/loka
LOKA_BUILD := $(BIN_DIR)/loka-build

# Default target
all: build

# Build all binaries for current platform.
# lokavm is a library (pkg/lokavm), linked into lokad directly.
build: $(LOKAD) $(LOKA_WORKER) $(LOKA_SUPERVISOR) $(LOKA_VMAGENT) $(LOKA_CLI)

$(LOKAD):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/lokad
ifeq ($(UNAME_S),Darwin)
	@printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>' > /tmp/lokad.entitlements
	@codesign --entitlements /tmp/lokad.entitlements --force -s - $@ 2>/dev/null || true
	@rm -f /tmp/lokad.entitlements
endif

$(LOKA_WORKER):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-worker

$(LOKA_SUPERVISOR):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-supervisor

$(LOKA_VMAGENT):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-vmagent

$(LOKA_CLI):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka

$(LOKA_BUILD):
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $@ ./cmd/loka-build
ifeq ($(UNAME_S),Darwin)
	@printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>' > /tmp/loka-build.entitlements
	@codesign --entitlements /tmp/loka-build.entitlements --force -s - $@ 2>/dev/null || true
	@rm -f /tmp/loka-build.entitlements
endif

# Build for Linux amd64 (cross-compile)
build-linux-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/lokad ./cmd/lokad
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-worker ./cmd/loka-worker
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-supervisor ./cmd/loka-supervisor
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka-vmagent ./cmd/loka-vmagent
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-amd64/loka ./cmd/loka

# Build for Linux arm64 (cross-compile)
build-linux-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/lokad ./cmd/lokad
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka-worker ./cmd/loka-worker
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka-supervisor ./cmd/loka-supervisor
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka-vmagent ./cmd/loka-vmagent
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/loka ./cmd/loka

# Build for all Linux architectures
build-linux: build-linux-amd64 build-linux-arm64

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

# Test metrics system (TSDB, parser, recorder, scraper, alerting, API)
test-metrics:
	$(GO) test -v -count=1 ./internal/metrics/ ./internal/controlplane/metrics/... ./internal/worker/metrics/ ./internal/loka/metrics/

# Test logging system (LogQL, store, handler, audit, scraper, API)
test-logging:
	$(GO) test -v -count=1 ./internal/controlplane/logging/... ./internal/worker/logbuffer/ ./internal/loka/logs/

# Test both metrics + logging
test-observability: test-metrics test-logging

# Install Cloud Hypervisor (Linux VMM backend)
install-cloud-hypervisor:
ifeq ($(UNAME_S),Linux)
	@echo "==> Installing Cloud Hypervisor $(CH_VERSION)"
	@CH_ARCH=$$(uname -m); \
	TMP=$$(mktemp -d); \
	CH_URL="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/cloud-hypervisor-static-$$CH_ARCH"; \
	echo "  Downloading $$CH_URL ..."; \
	curl -fsSL "$$CH_URL" -o "$$TMP/cloud-hypervisor" && \
	chmod +x "$$TMP/cloud-hypervisor" && \
	sudo install -m 755 "$$TMP/cloud-hypervisor" /usr/local/bin/cloud-hypervisor && \
	echo "  cloud-hypervisor → /usr/local/bin/cloud-hypervisor" && \
	rm -rf "$$TMP" || \
	(echo "  Failed to download. Install manually from https://github.com/cloud-hypervisor/cloud-hypervisor/releases"; rm -rf "$$TMP"; exit 1)
else
	@echo "  Cloud Hypervisor is Linux-only (macOS uses Apple Virtualization Framework)"
endif

# Install VM assets (kernel + initramfs) to ~/.loka/vm/
install-vm-assets:
	@mkdir -p $(HOME)/.loka/vm
	@if [ -f build/vmlinux-lokavm ]; then \
		cp build/vmlinux-lokavm $(HOME)/.loka/vm/vmlinux-lokavm; \
		echo "  kernel → ~/.loka/vm/vmlinux-lokavm"; \
	else \
		echo "  ! build/vmlinux-lokavm not found — run 'make kernel' first"; \
	fi
	@if [ -f build/initramfs.cpio.gz ]; then \
		cp build/initramfs.cpio.gz $(HOME)/.loka/vm/initramfs.cpio.gz; \
		echo "  initramfs → ~/.loka/vm/initramfs.cpio.gz"; \
	else \
		echo "  ! build/initramfs.cpio.gz not found — run 'make kernel' first"; \
	fi

# Install LOKA locally from source (no sudo required).
# Installs binaries to ~/.loka/bin/, kernel + initramfs to ~/.loka/vm/.
# Add ~/.loka/bin to your PATH.
INSTALL_DIR = $(HOME)/.loka/bin
SYMLINK_DIR ?= $(HOME)/.local/bin
install: build
	@echo "==> Installing LOKA"
	@mkdir -p $(INSTALL_DIR) $(SYMLINK_DIR)
	install -m 755 $(LOKA_CLI) $(INSTALL_DIR)/loka
	install -m 755 $(LOKAD) $(INSTALL_DIR)/lokad
	install -m 755 $(LOKA_SUPERVISOR) $(INSTALL_DIR)/loka-supervisor
	@if [ -f bin/loka-proxy ]; then install -m 755 bin/loka-proxy $(INSTALL_DIR)/loka-proxy; fi
	@# Install Linux supervisor for VM injection (if cross-compiled).
	@if [ -f bin/linux-arm64/loka-supervisor ]; then \
		mkdir -p $(INSTALL_DIR)/linux-arm64; \
		install -m 755 bin/linux-arm64/loka-supervisor $(INSTALL_DIR)/linux-arm64/loka-supervisor; \
		echo "  linux supervisor → $(INSTALL_DIR)/linux-arm64/"; \
	fi
	@if [ -f bin/linux-amd64/loka-supervisor ]; then \
		mkdir -p $(INSTALL_DIR)/linux-amd64; \
		install -m 755 bin/linux-amd64/loka-supervisor $(INSTALL_DIR)/linux-amd64/loka-supervisor; \
	fi
ifeq ($(UNAME_S),Darwin)
	@# Sign lokad with VZ entitlement (needed for Apple Virtualization Framework).
	@codesign --entitlements /dev/stdin --force -s - $(INSTALL_DIR)/lokad 2>/dev/null <<< \
		'<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>' || true
endif
	@# Symlink to ~/.local/bin for PATH access.
	@ln -sf $(INSTALL_DIR)/loka $(SYMLINK_DIR)/loka
	@ln -sf $(INSTALL_DIR)/lokad $(SYMLINK_DIR)/lokad
	@echo "  binaries → $(INSTALL_DIR)/"
	@echo "  symlinks → $(SYMLINK_DIR)/"
	@# Install kernel + initramfs to ~/.loka/vm/.
	@mkdir -p $(HOME)/.loka/vm
	@if [ -f build/vmlinux-lokavm ]; then \
		cp build/vmlinux-lokavm $(HOME)/.loka/vm/vmlinux-lokavm; \
		echo "  kernel  → ~/.loka/vm/vmlinux-lokavm"; \
	else \
		echo "  ! build/vmlinux-lokavm not found — run 'make kernel' first"; \
	fi
	@if [ -f build/initramfs.cpio.gz ]; then \
		cp build/initramfs.cpio.gz $(HOME)/.loka/vm/initramfs.cpio.gz; \
		echo "  initramfs → ~/.loka/vm/initramfs.cpio.gz"; \
	else \
		echo "  ! build/initramfs.cpio.gz not found — run 'make initramfs' first"; \
	fi
ifeq ($(UNAME_S),Linux)
	@if ! command -v cloud-hypervisor >/dev/null 2>&1; then \
		echo "  Cloud Hypervisor not found — installing..."; \
		$(MAKE) install-cloud-hypervisor; \
	else \
		echo "  cloud-hypervisor already installed"; \
	fi
endif
	@# Ensure ~/.local/bin is on PATH.
	@if ! echo "$$PATH" | tr ':' '\n' | grep -qx "$(SYMLINK_DIR)"; then \
		echo ""; \
		echo "  Add to your shell profile:"; \
		echo "    export PATH=\"$(SYMLINK_DIR):\$$PATH\""; \
	fi
	@echo "  LOKA installed."

# Uninstall LOKA
uninstall:
	@echo "==> Uninstalling LOKA"
	-rm -f $(SYMLINK_DIR)/loka $(SYMLINK_DIR)/lokad
	-rm -rf $(HOME)/.loka
	@echo "  LOKA uninstalled"

# Create release package (tar.gz) with all files needed to run.
# Usage: make release GOOS=linux GOARCH=arm64
RELEASE_GOOS ?= $(shell go env GOOS)
RELEASE_GOARCH ?= $(shell go env GOARCH)
RELEASE_DIR := release/loka-$(RELEASE_GOOS)-$(RELEASE_GOARCH)

release: build
	@echo "==> Creating release package: loka-$(RELEASE_GOOS)-$(RELEASE_GOARCH).tar.gz"
	@rm -rf $(RELEASE_DIR)
	@mkdir -p $(RELEASE_DIR)/vm
	@# Binaries.
	@cp $(LOKA_CLI) $(RELEASE_DIR)/loka
	@cp $(LOKAD) $(RELEASE_DIR)/lokad
	@cp $(LOKA_SUPERVISOR) $(RELEASE_DIR)/loka-supervisor
	@# Kernel + initramfs (required for VM boot).
	@if [ -f build/vmlinux-lokavm ]; then \
		cp build/vmlinux-lokavm $(RELEASE_DIR)/vm/vmlinux-lokavm; \
	else \
		echo "  ⚠ build/vmlinux-lokavm not found — release package incomplete"; \
	fi
	@if [ -f build/initramfs.cpio.gz ]; then \
		cp build/initramfs.cpio.gz $(RELEASE_DIR)/vm/initramfs.cpio.gz; \
	else \
		echo "  ⚠ build/initramfs.cpio.gz not found — release package incomplete"; \
	fi
	@# Install script.
	@cp scripts/install.sh $(RELEASE_DIR)/install.sh
	@# Pack.
	@cd release && tar -czf loka-$(RELEASE_GOOS)-$(RELEASE_GOARCH).tar.gz loka-$(RELEASE_GOOS)-$(RELEASE_GOARCH)/
	@rm -rf $(RELEASE_DIR)
	@ls -lh release/loka-$(RELEASE_GOOS)-$(RELEASE_GOARCH).tar.gz
	@echo "  Release package ready"

# Build custom Linux kernel for lokavm (cross-compiled on host)
kernel: $(LOKA_BUILD)
	@if [ -f build/vmlinux-lokavm ]; then \
		echo "  kernel exists (build/vmlinux-lokavm), skipping. Delete to rebuild."; \
	else \
		$(LOKA_BUILD) kernel --arch=$(UNAME_M); \
	fi

# Build initramfs for lokavm (built on host)
initramfs: $(LOKA_BUILD)
	@if [ -f build/initramfs.cpio.gz ]; then \
		echo "  initramfs exists (build/initramfs.cpio.gz), skipping. Delete to rebuild."; \
	else \
		$(LOKA_BUILD) initramfs --arch=$(UNAME_M); \
	fi

# Build both kernel and initramfs
kernel-all: $(LOKA_BUILD)
	$(LOKA_BUILD) all --arch=$(UNAME_M)

# Check kernel.org for latest stable and update pinned version
kernel-update:
	@echo "==> Checking latest stable kernel..."
	@LATEST=$$(curl -s https://www.kernel.org/ | grep -oP 'linux-\K[0-9]+\.[0-9]+\.[0-9]+' | head -1) && \
	echo "    Latest: $$LATEST" && \
	sed -i.bak "s/^defaultKernelVersion = .*/defaultKernelVersion = \"$$LATEST\"/" cmd/loka-build/main.go && \
	rm -f cmd/loka-build/main.go.bak && \
	echo "    Updated to $$LATEST"

# Run E2E test suite (native — requires KVM on Linux or VZ on macOS)
e2e-test: build
	bash scripts/e2e-test.sh

# Run specific E2E test sections (e.g., make e2e-test-section SECTIONS=25,26)
e2e-test-section: build
	LOKA_E2E_SECTIONS=$(SECTIONS) bash scripts/e2e-test.sh

# Run E2E test suite in a Linux VM (from macOS or CI — uses loka-build)
e2e-test-linux: $(LOKA_BUILD) build-linux-arm64
	$(LOKA_BUILD) e2e --arch=arm64

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
	@echo "Build:"
	@echo "  build                Build all binaries for current platform"
	@echo "  build-linux          Cross-compile for Linux (amd64 + arm64)"
	@echo "  build-all            Build for all platforms"
	@echo "  install              Build + install locally (~/.loka/bin/)"
	@echo "  uninstall            Remove LOKA and all data"
	@echo "  release              Create release tar.gz"
	@echo "  clean                Remove build artifacts"
	@echo ""
	@echo "Test:"
	@echo "  test                 Run all unit tests"
	@echo "  test-metrics         Run metrics system tests (TSDB, parser, alerting)"
	@echo "  test-logging         Run logging system tests (LogQL, store, handler)"
	@echo "  test-observability   Run both metrics + logging tests"
	@echo "  e2e-test             Run full E2E test suite"
	@echo "  e2e-test-section     Run specific E2E sections (SECTIONS=25,26)"
	@echo "  e2e-test-linux       Run E2E in Linux VM (from macOS/CI)"
	@echo ""
	@echo "VM:"
	@echo "  kernel               Build Linux kernel for lokavm"
	@echo "  initramfs            Build initramfs for lokavm"
	@echo "  kernel-all           Build both kernel + initramfs"
	@echo "  install-vm-assets    Install kernel + initramfs to ~/.loka/vm/"
	@echo "  install-cloud-hypervisor  Install Cloud Hypervisor (Linux)"
	@echo ""
	@echo "Other:"
	@echo "  proto                Generate protobuf code"
	@echo "  lint                 Run linter"
	@echo "  fmt                  Format code"
	@echo "  help                 Show this help"
	@echo ""
	@echo "Roles (lokad --role=<role>):"
	@echo "  all (default)        Full node: CP + worker + metrics + logs"
	@echo "  controlplane         CP only (no embedded worker)"
	@echo "  metrics              Standalone metrics TSDB + query API"
	@echo "  logs                 Standalone log store + query API"
	@echo "  observability        Metrics + logs (no CP components)"
	@echo "  gateway              Domain proxy only"
