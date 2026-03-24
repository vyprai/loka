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

  # Check if we need sudo: install dir writable AND no existing binaries in protected paths.
  local needs_sudo=false
  if ! { [ -w "$INSTALL_DIR" ] || mkdir -p "$INSTALL_DIR" 2>/dev/null; }; then
    needs_sudo=true
  fi
  # Also need sudo if existing binaries are in a non-writable location.
  for bin in loka lokad loka-worker loka-supervisor; do
    if [ -f "${INSTALL_DIR}/$bin" ] && [ ! -w "${INSTALL_DIR}/$bin" ]; then
      needs_sudo=true
      break
    fi
  done
  if [ "$needs_sudo" = false ]; then
    SUDO=""
    return
  fi

  if command -v sudo &>/dev/null; then
    info "This installer needs elevated privileges to install binaries to ${INSTALL_DIR}."
    if ! sudo -v 2>/dev/null; then
      fail "sudo access required. Run with sudo or as root."
    fi
    SUDO="sudo"
    # Keep sudo alive for the duration of the script.
    (while true; do sudo -n true 2>/dev/null; sleep 50; done) &
    SUDO_KEEPALIVE_PID=$!
    trap 'kill $SUDO_KEEPALIVE_PID 2>/dev/null; wait $SUDO_KEEPALIVE_PID 2>/dev/null' EXIT
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

  local kernel_url="https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${fc_arch}/vmlinux-5.10.bin"
  info "Downloading Linux kernel..."
  $SUDO curl -fsSL "$kernel_url" -o "${kernel_dir}/vmlinux"
  ok "kernel → ${kernel_dir}/vmlinux"

  # Also symlink to default dev path used by lokad.
  local dev_kernel_dir="/tmp/loka-data/artifacts/kernel"
  $SUDO mkdir -p "$dev_kernel_dir"
  $SUDO ln -sf "${kernel_dir}/vmlinux" "$dev_kernel_dir/vmlinux" 2>/dev/null || true
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

    # Use custom LOKA image from GitHub releases if available,
    # otherwise fall back to stock Alpine cloud image + provision script.
    local img_base="https://github.com/vyprai/loka/releases/latest/download"
    local use_custom=false

    if curl -fsSL -o /dev/null -w '%{http_code}' "${img_base}/loka-lima-arm64.iso" 2>/dev/null | grep -q "^200\|^302"; then
      use_custom=true
      info "Using pre-built LOKA image (~108MB, binaries included)"
    else
      info "Using Alpine cloud image (will provision on first boot)"
    fi

    local lima_config
    lima_config=$(mktemp)

    if [ "$use_custom" = true ]; then
      # Custom image: Docker, LOKA binaries, KVM pre-installed. No provision needed.
      cat > "$lima_config" <<LIMAEOF
# LOKA Lima VM — pre-built image with Docker + LOKA

vmType: vz
nestedVirtualization: true

containerd:
  system: false
  user: false

images:
  - location: "${img_base}/loka-lima-arm64.iso"
    arch: "aarch64"
  - location: "${img_base}/loka-lima-amd64.iso"
    arch: "x86_64"

cpus: 4
memory: "8GiB"
disk: "50GiB"

mounts:
  - location: "~"
    writable: true

portForwards:
  - guestPort: 6840
    hostPort: 6840
  - guestPort: 6841
    hostPort: 6841

provision:
  - mode: system
    script: |
      #!/bin/sh
      # LOKA ISO: binaries + firecracker + kernel pre-installed.
      # Provision: enable KVM, link kernel, install Docker, create rootfs.
      #!/bin/sh
      set -eux
      [ -e /dev/kvm ] && chmod 666 /dev/kvm || true

      # Link kernel to paths lokad expects.
      mkdir -p /var/loka/kernel /tmp/loka-data/kernel /tmp/loka-data/rootfs /tmp/loka-data/objstore
      [ -f /usr/share/loka/vmlinux ] && cp /usr/share/loka/vmlinux /var/loka/kernel/vmlinux
      [ -f /var/loka/kernel/vmlinux ] && ln -sf /var/loka/kernel/vmlinux /tmp/loka-data/kernel/vmlinux

      # Install Docker.
      if ! command -v docker >/dev/null 2>&1; then
        apk add --no-cache docker >/dev/null 2>&1
        rc-update add docker default 2>/dev/null || true
      fi
      service docker start 2>/dev/null || true

      # Create default rootfs if missing.
      if [ ! -f /tmp/loka-data/rootfs/rootfs.ext4 ] && command -v docker >/dev/null 2>&1; then
        docker pull alpine:latest >/dev/null 2>&1 || true
        CID=$(docker create alpine:latest 2>/dev/null) || true
        if [ -n "$CID" ]; then
          docker export $CID > /tmp/rootfs.tar
          docker rm $CID >/dev/null
          dd if=/dev/zero of=/tmp/loka-data/rootfs/rootfs.ext4 bs=1M count=128 2>/dev/null
          mkfs.ext4 -F /tmp/loka-data/rootfs/rootfs.ext4 >/dev/null 2>&1
          mkdir -p /tmp/mnt-rootfs
          mount -o loop /tmp/loka-data/rootfs/rootfs.ext4 /tmp/mnt-rootfs
          tar xf /tmp/rootfs.tar -C /tmp/mnt-rootfs 2>/dev/null
          [ -f /usr/local/bin/loka-supervisor ] && cp /usr/local/bin/loka-supervisor /tmp/mnt-rootfs/usr/local/bin/loka-supervisor && chmod +x /tmp/mnt-rootfs/usr/local/bin/loka-supervisor
          umount /tmp/mnt-rootfs; rmdir /tmp/mnt-rootfs; rm -f /tmp/rootfs.tar
        fi
      fi
LIMAEOF
    else
      # Stock Alpine: needs full provision.
      cat > "$lima_config" <<'LIMAEOF'
# LOKA Lima VM — Alpine Linux with KVM for Firecracker

vmType: vz
nestedVirtualization: true

containerd:
  system: false
  user: false

images:
  - location: "https://dl-cdn.alpinelinux.org/alpine/v3.23/releases/cloud/nocloud_alpine-3.23.3-aarch64-uefi-cloudinit-r0.qcow2"
    arch: "aarch64"
  - location: "https://dl-cdn.alpinelinux.org/alpine/v3.23/releases/cloud/nocloud_alpine-3.23.3-x86_64-uefi-cloudinit-r0.qcow2"
    arch: "x86_64"

cpus: 4
memory: "8GiB"
disk: "50GiB"

mounts:
  - location: "~"
    writable: true

portForwards:
  - guestPort: 6840
    hostPort: 6840
  - guestPort: 6841
    hostPort: 6841

provision:
  - mode: system
    script: |
      #!/bin/sh
      set -eux
      apk add --no-cache curl iptables iproute2 e2fsprogs docker
      rc-update add docker default 2>/dev/null || true
      service docker start 2>/dev/null || true
      [ -e /dev/kvm ] && chmod 666 /dev/kvm || true
      curl -fsSL https://vyprai.github.io/loka/install.sh | sh
LIMAEOF
    fi

    limactl create --name="$LIMA_INSTANCE" --tty=false "$lima_config" 2>&1 | grep -v "Failed to open TUI"
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

# ── Uninstall previous installation ─────────────────────

uninstall_previous() {
  local found=false

  # Check for existing LOKA binaries.
  for bin in loka lokad loka-worker loka-supervisor; do
    if [ -f "${INSTALL_DIR}/$bin" ]; then
      found=true
      break
    fi
  done

  if [ "$found" = false ]; then
    return
  fi

  echo ""
  info "Existing LOKA installation detected"

  # Stop running lokad (Linux: direct process, macOS: via CLI or Lima).
  if pgrep -x lokad &>/dev/null; then
    info "Stopping running lokad..."
    if command -v loka &>/dev/null; then
      loka deploy down 2>/dev/null || true
      sleep 1
    fi
    # If still running, kill directly.
    if pgrep -x lokad &>/dev/null; then
      $SUDO pkill -x lokad 2>/dev/null || true
      sleep 1
    fi
    ok "lokad stopped"
  fi

  # On macOS: stop Lima VM if running.
  if [ "$OS" = "darwin" ] && command -v limactl &>/dev/null; then
    if limactl list 2>/dev/null | grep "$LIMA_INSTANCE" | grep -q Running; then
      info "Stopping Lima VM '${LIMA_INSTANCE}'..."
      limactl stop "$LIMA_INSTANCE" 2>/dev/null || true
      ok "Lima VM stopped"
    fi
    if limactl list -q 2>/dev/null | grep -q "^${LIMA_INSTANCE}$"; then
      info "Removing Lima VM '${LIMA_INSTANCE}'..."
      limactl delete "$LIMA_INSTANCE" --force 2>/dev/null || true
      ok "Lima VM removed"
    fi
  fi

  # Remove binaries.
  info "Removing old binaries..."
  for bin in loka lokad loka-worker loka-supervisor; do
    if [ -f "${INSTALL_DIR}/$bin" ]; then
      $SUDO rm -f "${INSTALL_DIR}/$bin"
      ok "Removed ${INSTALL_DIR}/$bin"
    fi
  done

  # Remove Firecracker binary (Linux only).
  if [ "$OS" = "linux" ] && [ -f "${INSTALL_DIR}/firecracker" ]; then
    $SUDO rm -f "${INSTALL_DIR}/firecracker"
    ok "Removed ${INSTALL_DIR}/firecracker"
  fi

  # Remove data directory.
  if [ -d "$DATA_DIR" ]; then
    info "Removing data directory ${DATA_DIR}..."
    $SUDO rm -rf "$DATA_DIR"
    ok "Removed ${DATA_DIR}"
  fi

  # Remove temp data.
  if [ -d "/tmp/loka-data" ]; then
    $SUDO rm -rf /tmp/loka-data
    ok "Removed /tmp/loka-data"
  fi

  # Remove config (Linux only).
  if [ "$OS" = "linux" ] && [ -d "/etc/loka" ]; then
    $SUDO rm -rf /etc/loka
    ok "Removed /etc/loka"
  fi

  # Remove client config.
  if [ -d "$HOME/.loka" ]; then
    rm -rf "$HOME/.loka"
    ok "Removed ~/.loka"
  fi

  # Remove shell completions.
  $SUDO rm -f /etc/bash_completion.d/loka 2>/dev/null || true
  $SUDO rm -f /usr/local/share/zsh/site-functions/_loka 2>/dev/null || true

  echo ""
  ok "Previous installation cleaned up"
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

  uninstall_previous
  echo ""

  case "$OS" in
    linux)  install_linux ;;
    darwin) install_macos ;;
  esac
}

main "$@"
