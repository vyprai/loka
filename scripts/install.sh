#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
#  LOKA Installer
#  Usage: curl -fsSL https://rizqme.github.io/loka/install.sh | bash
# ──────────────────────────────────────────────────────────
set -euo pipefail

VERSION="${LOKA_VERSION:-latest}"
INSTALL_DIR="${LOKA_INSTALL_DIR:-/usr/local/bin}"
DATA_DIR="${LOKA_DATA_DIR:-/var/loka}"
FC_VERSION="${FC_VERSION:-v1.10.1}"

# ── Helpers ───────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${CYAN}==>${NC} $*"; }
ok()    { echo -e "${GREEN}  ✓${NC} $*"; }
warn()  { echo -e "${YELLOW}  !${NC} $*"; }
fail()  { echo -e "${RED}  ✗ $*${NC}"; exit 1; }

need_cmd() {
  if ! command -v "$1" &>/dev/null; then
    fail "$1 is required but not installed"
  fi
}

# ── Detect platform ──────────────────────────────────────

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)

  case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "Unsupported architecture: $ARCH" ;;
  esac

  case "$OS" in
    linux) ;;
    darwin)
      warn "macOS detected — Firecracker requires Linux with KVM."
      warn "LOKA binaries will be installed, but you need Lima to run VMs."
      warn "Run 'loka setup-lima' after installation."
      ;;
    *) fail "Unsupported OS: $OS" ;;
  esac

  PLATFORM="${OS}-${ARCH}"
}

# ── Check prerequisites ─────────────────────────────────

check_prereqs() {
  need_cmd curl
  need_cmd tar

  if [ "$OS" = "linux" ]; then
    if [ ! -e /dev/kvm ]; then
      warn "/dev/kvm not found — Firecracker requires KVM"
      warn "Enable KVM or run inside a VM with nested virtualization"
    else
      ok "KVM available"
    fi
  fi

  if command -v docker &>/dev/null; then
    ok "Docker available"
  else
    warn "Docker not found — needed for 'loka image pull'"
  fi
}

# ── Install LOKA binaries ───────────────────────────────

install_loka() {
  info "Installing LOKA binaries to ${INSTALL_DIR}"

  local url
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/rizqme/loka/releases/latest/download/loka-${PLATFORM}.tar.gz"
  else
    url="https://github.com/rizqme/loka/releases/download/${VERSION}/loka-${PLATFORM}.tar.gz"
  fi

  local tmp
  tmp=$(mktemp -d)
  trap "rm -rf $tmp" EXIT

  info "Downloading loka-${PLATFORM}.tar.gz ..."

  if curl -fsSL "$url" -o "$tmp/loka.tar.gz" 2>/dev/null; then
    tar -xzf "$tmp/loka.tar.gz" -C "$tmp"
  else
    # If release doesn't exist yet, build from source.
    warn "Pre-built binary not found. Building from source..."
    need_cmd go
    need_cmd git

    if [ ! -d "$tmp/loka-src" ]; then
      git clone --depth 1 https://github.com/rizqme/loka "$tmp/loka-src" 2>/dev/null
    fi
    cd "$tmp/loka-src"
    GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/lokad" ./cmd/lokad
    GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/loka-worker" ./cmd/loka-worker
    GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/loka-supervisor" ./cmd/loka-supervisor
    GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/loka" ./cmd/loka
    cd - >/dev/null
  fi

  # Install binaries.
  local bins=("lokad" "loka-worker" "loka-supervisor" "loka")
  for bin in "${bins[@]}"; do
    if [ -f "$tmp/$bin" ]; then
      sudo install -m 755 "$tmp/$bin" "${INSTALL_DIR}/$bin"
      ok "$bin → ${INSTALL_DIR}/$bin"
    fi
  done
}

# ── Install Firecracker ─────────────────────────────────

install_firecracker() {
  if [ "$OS" != "linux" ]; then
    warn "Skipping Firecracker on $OS (requires Linux)"
    return
  fi

  info "Installing Firecracker ${FC_VERSION}"

  local fc_arch
  case "$ARCH" in
    amd64) fc_arch="x86_64" ;;
    arm64) fc_arch="aarch64" ;;
  esac

  local tmp
  tmp=$(mktemp -d)

  local fc_url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${fc_arch}.tgz"

  curl -fsSL "$fc_url" | tar -xz -C "$tmp"
  sudo install -m 755 "$tmp/release-${FC_VERSION}-${fc_arch}/firecracker-${FC_VERSION}-${fc_arch}" "${INSTALL_DIR}/firecracker"
  ok "firecracker → ${INSTALL_DIR}/firecracker"

  rm -rf "$tmp"

  # Fetch kernel.
  local kernel_dir="${DATA_DIR}/kernel"
  sudo mkdir -p "$kernel_dir"

  local kernel_url="https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${fc_arch}/vmlinux-5.10.217"
  info "Downloading Linux kernel..."
  sudo curl -fsSL "$kernel_url" -o "${kernel_dir}/vmlinux"
  ok "kernel → ${kernel_dir}/vmlinux"
}

# ── Create data directories ─────────────────────────────

setup_dirs() {
  info "Creating data directories"
  sudo mkdir -p "${DATA_DIR}"/{artifacts,worker}
  sudo chmod 755 "${DATA_DIR}"
  ok "${DATA_DIR}"
}

# ── Create default config ───────────────────────────────

setup_config() {
  local config_dir="/etc/loka"
  sudo mkdir -p "$config_dir"

  if [ ! -f "$config_dir/controlplane.yaml" ]; then
    info "Writing default config"
    sudo tee "$config_dir/controlplane.yaml" >/dev/null << YAML
mode: single
listen_addr: ":8080"
database:
  driver: sqlite
  dsn: "${DATA_DIR}/loka.db"
coordinator:
  type: local
objectstore:
  type: local
  path: "${DATA_DIR}/artifacts"
scheduler:
  strategy: spread
logging:
  format: text
  level: info
YAML
    ok "$config_dir/controlplane.yaml"
  else
    ok "Config already exists, skipping"
  fi
}

# ── Shell completion ────────────────────────────────────

setup_completion() {
  if command -v loka &>/dev/null; then
    if [ -d /etc/bash_completion.d ]; then
      loka completion bash | sudo tee /etc/bash_completion.d/loka >/dev/null 2>&1 && ok "bash completion"
    fi
    if [ -d /usr/local/share/zsh/site-functions ]; then
      loka completion zsh | sudo tee /usr/local/share/zsh/site-functions/_loka >/dev/null 2>&1 && ok "zsh completion"
    fi
  fi
}

# ── Main ────────────────────────────────────────────────

main() {
  echo ""
  echo -e "${BOLD}  LOKA Installer${NC}"
  echo -e "  Session-based microVM execution for AI agents"
  echo ""

  detect_platform
  info "Platform: ${PLATFORM}"
  echo ""

  check_prereqs
  echo ""

  install_loka
  echo ""

  install_firecracker
  echo ""

  setup_dirs
  setup_config
  setup_completion
  echo ""

  echo -e "${GREEN}${BOLD}  Installation complete!${NC}"
  echo ""
  echo "  Quick start:"
  echo ""
  echo -e "    ${CYAN}lokad${NC}                                    # Start the server"
  echo -e "    ${CYAN}loka image pull python:3.12-slim${NC}      # Pull an image"
  echo -e "    ${CYAN}loka session create --image python:3.12-slim${NC}"
  echo -e "    ${CYAN}loka exec <id> -- python3 -c \"print(42)\"${NC}"
  echo ""
  echo "  Docs: https://docs.loka.dev"
  echo ""

  if [ "$OS" = "darwin" ]; then
    echo -e "  ${YELLOW}macOS: Run 'make setup-lima' for a KVM-enabled Linux VM${NC}"
    echo ""
  fi
}

main "$@"
