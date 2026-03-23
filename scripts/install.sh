#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
#  LOKA Installer
#  Usage: curl -fsSL https://vyprai.github.io/loka/install.sh | bash
#
#  On Linux:  installs binaries, Firecracker, kernel, configs
#  On macOS:  installs CLI + sets up a Lima VM with KVM for LOKA
# ──────────────────────────────────────────────────────────
set -euo pipefail

VERSION="${LOKA_VERSION:-latest}"
INSTALL_DIR="${LOKA_INSTALL_DIR:-/usr/local/bin}"
DATA_DIR="${LOKA_DATA_DIR:-/var/loka}"
FC_VERSION="${FC_VERSION:-v1.10.1}"
LIMA_INSTANCE="loka"

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

# Determine how to run privileged commands.
SUDO=""
setup_sudo() {
  if [ "$(id -u)" -eq 0 ]; then
    SUDO=""
    return
  fi

  if command -v sudo &>/dev/null; then
    # Validate sudo access upfront so it doesn't prompt mid-install.
    info "This installer needs elevated privileges to install binaries and configure the system."
    if ! sudo -v 2>/dev/null; then
      fail "sudo access required. Run with sudo or as root."
    fi
    SUDO="sudo"
    # Keep sudo alive for the duration of the script.
    (while true; do sudo -n true; sleep 50; done) &
    SUDO_KEEPALIVE_PID=$!
    trap "kill $SUDO_KEEPALIVE_PID 2>/dev/null; rm -rf ${_CLEANUP_TMP:-/dev/null}" EXIT
  else
    fail "sudo is required but not found. Run as root instead."
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
    linux|darwin) ;;
    *) fail "Unsupported OS: $OS" ;;
  esac

  PLATFORM="${OS}-${ARCH}"
}

# ── Download LOKA binaries ───────────────────────────────

download_binaries() {
  local target_os="${1:-$OS}"
  local target_arch="${2:-$ARCH}"
  local target_dir="${3:-}"
  local platform="${target_os}-${target_arch}"

  local url
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/vyprai/loka/releases/latest/download/loka-${platform}.tar.gz"
  else
    url="https://github.com/vyprai/loka/releases/download/${VERSION}/loka-${platform}.tar.gz"
  fi

  local tmp
  tmp=$(mktemp -d)

  info "Downloading loka-${platform}.tar.gz ..."

  if curl -fsSL "$url" -o "$tmp/loka.tar.gz" 2>/dev/null; then
    tar -xzf "$tmp/loka.tar.gz" -C "$tmp"
  else
    warn "Pre-built binary not found. Building from source..."
    need_cmd go
    need_cmd git

    git clone --depth 1 https://github.com/vyprai/loka "$tmp/loka-src" 2>/dev/null
    cd "$tmp/loka-src"
    GOOS=$target_os GOARCH=$target_arch go build -trimpath -ldflags "-s -w" -o "$tmp/lokad" ./cmd/lokad
    GOOS=$target_os GOARCH=$target_arch go build -trimpath -ldflags "-s -w" -o "$tmp/loka-worker" ./cmd/loka-worker
    GOOS=$target_os GOARCH=$target_arch go build -trimpath -ldflags "-s -w" -o "$tmp/loka-supervisor" ./cmd/loka-supervisor
    GOOS=$target_os GOARCH=$target_arch go build -trimpath -ldflags "-s -w" -o "$tmp/loka" ./cmd/loka
    cd - >/dev/null
  fi

  if [ -n "$target_dir" ]; then
    cp "$tmp"/{lokad,loka-worker,loka-supervisor,loka} "$target_dir/" 2>/dev/null || true
  else
    local bins=("lokad" "loka-worker" "loka-supervisor" "loka")
    for bin in "${bins[@]}"; do
      if [ -f "$tmp/$bin" ]; then
        $SUDO install -m 755 "$tmp/$bin" "${INSTALL_DIR}/$bin"
        ok "$bin → ${INSTALL_DIR}/$bin"
      fi
    done
  fi

  rm -rf "$tmp"
}

# ── Install Firecracker (Linux only) ────────────────────

install_firecracker() {
  if [ "$OS" != "linux" ]; then
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
  $SUDO install -m 755 "$tmp/release-${FC_VERSION}-${fc_arch}/firecracker-${FC_VERSION}-${fc_arch}" "${INSTALL_DIR}/firecracker"
  ok "firecracker → ${INSTALL_DIR}/firecracker"

  rm -rf "$tmp"

  # Fetch kernel.
  local kernel_dir="${DATA_DIR}/kernel"
  $SUDO mkdir -p "$kernel_dir"

  local kernel_url="https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${fc_arch}/vmlinux-5.10.217"
  info "Downloading Linux kernel..."
  $SUDO curl -fsSL "$kernel_url" -o "${kernel_dir}/vmlinux"
  ok "kernel → ${kernel_dir}/vmlinux"
}

# ── Install Linux dependencies ───────────────────────────

install_linux_deps() {
  info "Checking dependencies..."

  # Detect package manager.
  local pkg=""
  if command -v apt-get &>/dev/null; then
    pkg="apt"
  elif command -v dnf &>/dev/null; then
    pkg="dnf"
  elif command -v yum &>/dev/null; then
    pkg="yum"
  elif command -v apk &>/dev/null; then
    pkg="apk"
  fi

  # Packages we need.
  local missing=()

  # iptables — needed for network policy enforcement inside VMs.
  if ! command -v iptables &>/dev/null; then
    missing+=("iptables")
  else
    ok "iptables"
  fi

  # iproute2 (ip command) — needed for network setup.
  if ! command -v ip &>/dev/null; then
    missing+=("iproute2")
  else
    ok "iproute2"
  fi

  # e2fsprogs (mkfs.ext4) — needed for rootfs creation.
  if ! command -v mkfs.ext4 &>/dev/null; then
    missing+=("e2fsprogs")
  else
    ok "e2fsprogs"
  fi

  # KVM.
  if [ ! -e /dev/kvm ]; then
    warn "/dev/kvm not found — Firecracker requires KVM"
    warn "Enable KVM or run inside a VM with nested virtualization"
    # Try to load kvm module.
    $SUDO modprobe kvm 2>/dev/null || true
    $SUDO modprobe kvm_intel 2>/dev/null || $SUDO modprobe kvm_amd 2>/dev/null || true
    if [ -e /dev/kvm ]; then
      ok "KVM loaded"
    fi
  else
    ok "KVM available"
  fi

  # Docker — optional but recommended.
  if command -v docker &>/dev/null; then
    ok "Docker available"
  else
    warn "Docker not found — needed for 'loka image pull'"
    info "Install Docker: https://docs.docker.com/engine/install/"
  fi

  # Install missing packages.
  if [ ${#missing[@]} -gt 0 ]; then
    info "Installing missing packages: ${missing[*]}"
    case "$pkg" in
      apt)
        $SUDO apt-get update -qq
        $SUDO apt-get install -y -qq "${missing[@]}"
        ;;
      dnf)
        $SUDO dnf install -y -q "${missing[@]}"
        ;;
      yum)
        $SUDO yum install -y -q "${missing[@]}"
        ;;
      apk)
        $SUDO apk add --quiet "${missing[@]}"
        ;;
      *)
        warn "Unknown package manager — install manually: ${missing[*]}"
        ;;
    esac
    for dep in "${missing[@]}"; do
      ok "$dep installed"
    done
  fi

  # Ensure current user can access /dev/kvm.
  if [ -e /dev/kvm ] && [ ! -w /dev/kvm ]; then
    info "Adding current user to kvm group..."
    $SUDO usermod -aG kvm "$(whoami)" 2>/dev/null || true
    $SUDO chmod 666 /dev/kvm 2>/dev/null || true
    ok "KVM access granted"
  fi
}

# ── Linux install ────────────────────────────────────────

install_linux() {
  install_linux_deps
  echo ""

  info "Installing LOKA binaries to ${INSTALL_DIR}"
  download_binaries
  echo ""

  install_firecracker
  echo ""

  # Data dirs.
  info "Creating data directories"
  $SUDO mkdir -p "${DATA_DIR}"/{artifacts,worker,raft,tls}
  $SUDO chmod 755 "${DATA_DIR}"
  $SUDO chmod 700 "${DATA_DIR}/tls"
  ok "${DATA_DIR}"

  # Default config.
  local config_dir="/etc/loka"
  $SUDO mkdir -p "$config_dir"
  if [ ! -f "$config_dir/controlplane.yaml" ]; then
    info "Writing default config"
    $SUDO tee "$config_dir/controlplane.yaml" >/dev/null << YAML
role: all
mode: single
listen_addr: ":6840"
grpc_addr: ":6841"
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
YAML
    ok "$config_dir/controlplane.yaml"
  else
    ok "Config already exists, skipping"
  fi

  # Shell completion.
  if command -v loka &>/dev/null; then
    if [ -d /etc/bash_completion.d ]; then
      loka completion bash | $SUDO tee /etc/bash_completion.d/loka >/dev/null 2>&1 && ok "bash completion"
    fi
    if [ -d /usr/local/share/zsh/site-functions ]; then
      loka completion zsh | $SUDO tee /usr/local/share/zsh/site-functions/_loka >/dev/null 2>&1 && ok "zsh completion"
    fi
  fi

  echo ""
  echo -e "${GREEN}${BOLD}  Installation complete!${NC}"
  echo ""
  echo "  TLS is enabled by default (auto-generated certificates)."
  echo ""
  echo "  Quick start:"
  echo ""
  echo -e "    ${CYAN}loka deploy local${NC}                           # Start the server"
  echo -e "    ${CYAN}loka image pull python:3.12-slim${NC}        # Pull an image"
  echo -e "    ${CYAN}loka session create --image python:3.12-slim${NC}"
  echo -e "    ${CYAN}loka exec <id> -- python3 -c \"print(42)\"${NC}"
  echo -e "    ${CYAN}loka deploy down${NC}                            # Stop"
  echo ""
}

# ── macOS install (Lima) ─────────────────────────────────

install_macos() {
  info "macOS detected — setting up LOKA with Lima"
  echo ""
  info "Firecracker requires Linux with KVM."
  info "Lima creates a lightweight Linux VM on your Mac for this."
  echo ""

  # Step 1: Install the loka CLI locally (macOS binary).
  info "Installing loka CLI to ${INSTALL_DIR}"
  download_binaries
  echo ""

  # Shell completion.
  if command -v loka &>/dev/null; then
    if [ -d /usr/local/share/zsh/site-functions ]; then
      loka completion zsh | $SUDO tee /usr/local/share/zsh/site-functions/_loka >/dev/null 2>&1 && ok "zsh completion"
    fi
    local bash_comp_dir
    bash_comp_dir="$(brew --prefix 2>/dev/null)/etc/bash_completion.d" 2>/dev/null || true
    if [ -d "$bash_comp_dir" ]; then
      loka completion bash | tee "$bash_comp_dir/loka" >/dev/null 2>&1 && ok "bash completion"
    fi
  fi

  # Step 2: Install Lima if needed.
  if ! command -v limactl &>/dev/null; then
    info "Installing Lima via Homebrew..."
    if command -v brew &>/dev/null; then
      brew install lima
      ok "Lima installed"
    else
      fail "Homebrew not found. Install Lima manually: https://lima-vm.io"
    fi
  else
    ok "Lima already installed"
  fi

  # Step 3: Create Lima VM.
  if limactl list -q 2>/dev/null | grep -q "^${LIMA_INSTANCE}$"; then
    info "Lima instance '${LIMA_INSTANCE}' already exists"
    if ! limactl list 2>/dev/null | grep "$LIMA_INSTANCE" | grep -q "Running"; then
      info "Starting Lima instance..."
      limactl start "$LIMA_INSTANCE"
    fi
    ok "Lima instance running"
  else
    info "Creating Lima VM '${LIMA_INSTANCE}' (this takes a few minutes)..."
    echo ""

    local lima_config
    lima_config=$(mktemp)
    cat > "$lima_config" <<'LIMAEOF'
# LOKA Lima VM — Linux with KVM for Firecracker microVMs

images:
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
    arch: "x86_64"
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
    arch: "aarch64"

cpus: 4
memory: "8GiB"
disk: "50GiB"

mounts:
  - location: "~"
    writable: true

portForward:
  - guestPort: 6840
    hostPort: 6840
  - guestPort: 6841
    hostPort: 6841

provision:
  - mode: system
    script: |
      #!/bin/bash
      set -eux

      # Enable KVM.
      apt-get update -q
      apt-get install -y -q qemu-kvm curl
      chmod 666 /dev/kvm

      # Install Docker.
      if ! command -v docker &>/dev/null; then
        curl -fsSL https://get.docker.com | sh
        usermod -aG docker "${LIMA_CIDATA_USER}"
      fi

      # Install LOKA inside the VM.
      curl -fsSL https://vyprai.github.io/loka/install.sh | bash

      echo "LOKA is ready inside the Lima VM."
LIMAEOF

    limactl create --name="$LIMA_INSTANCE" "$lima_config"
    rm "$lima_config"

    info "Starting Lima VM..."
    limactl start "$LIMA_INSTANCE"
    ok "Lima VM ready"
  fi

  echo ""
  echo -e "${GREEN}${BOLD}  Installation complete!${NC}"
  echo ""
  echo "  LOKA runs inside a Lima VM with KVM support."
  echo "  Ports 6840 and 6841 are forwarded to localhost."
  echo ""
  echo "  Quick start:"
  echo ""
  echo -e "    ${CYAN}loka deploy local${NC}                           # Start LOKA (uses Lima automatically)"
  echo -e "    ${CYAN}loka image pull python:3.12-slim${NC}"
  echo -e "    ${CYAN}loka session create --image python:3.12-slim${NC}"
  echo -e "    ${CYAN}loka exec <id> -- python3 -c \"print(42)\"${NC}"
  echo -e "    ${CYAN}loka deploy down${NC}                            # Stop"
  echo ""
}

# ── Main ─────────────────────────────────────────────────

main() {
  echo ""
  echo -e "${BOLD}  LOKA Installer${NC}"
  echo -e "  Controlled execution environment for AI agents"
  echo ""

  detect_platform
  info "Platform: ${PLATFORM}"
  echo ""

  need_cmd curl
  need_cmd tar

  setup_sudo
  echo ""

  case "$OS" in
    linux)  install_linux ;;
    darwin) install_macos ;;
  esac
}

main "$@"
