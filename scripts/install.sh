#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
#  LOKA Installer
#
#  Online:  curl -fsSL https://vyprai.github.io/loka/install.sh | bash
#  Local:   ./install.sh --local /path/to/release/dir
#
#  Environment variables:
#    LOKA_VERSION       Release version (default: latest)
#    LOKA_INSTALL_DIR   Binary install dir (default: ~/.loka/bin)
#    LOKA_SYMLINK_DIR   Symlink dir on PATH (default: ~/.local/bin)
#    LOKA_LOCAL_DIR     Path to extracted release dir (skip download)
#    CH_VERSION         Cloud Hypervisor version (default: v44.0)
# ──────────────────────────────────────────────────────────
set -euo pipefail

VERSION="${LOKA_VERSION:-latest}"
INSTALL_DIR="${LOKA_INSTALL_DIR:-$HOME/.loka/bin}"
SYMLINK_DIR="${LOKA_SYMLINK_DIR:-$HOME/.local/bin}"
CH_VERSION="${CH_VERSION:-v44.0}"
LOCAL_DIR="${LOKA_LOCAL_DIR:-}"

# Parse --local flag.
while [ $# -gt 0 ]; do
  case "$1" in
    --local)
      LOCAL_DIR="$2"
      shift 2
      ;;
    --local=*)
      LOCAL_DIR="${1#--local=}"
      shift
      ;;
    *)
      shift
      ;;
  esac
done

# ── Helpers ───────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
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
    linux|darwin) ;;
    *) fail "Unsupported OS: $OS" ;;
  esac

  PLATFORM="${OS}-${ARCH}"
}

# ── Download LOKA binaries ───────────────────────────────

download_and_install() {
  local pkg_dir=""
  local tmp=""

  # If --local was given, use that directory directly (no download).
  if [ -n "$LOCAL_DIR" ]; then
    if [ ! -d "$LOCAL_DIR" ]; then
      fail "Local directory not found: $LOCAL_DIR"
    fi
    pkg_dir="$LOCAL_DIR"
    info "Installing from local directory: $LOCAL_DIR"
  else
    local platform="${OS}-${ARCH}"
    local url
    if [ "$VERSION" = "latest" ]; then
      url="https://github.com/vyprai/loka/releases/latest/download/loka-${platform}.tar.gz"
    else
      url="https://github.com/vyprai/loka/releases/download/${VERSION}/loka-${platform}.tar.gz"
    fi

    tmp=$(mktemp -d)

    info "Downloading loka-${platform}.tar.gz ..."

    if curl -fsSL "$url" -o "$tmp/loka.tar.gz" 2>/dev/null; then
      tar -xzf "$tmp/loka.tar.gz" -C "$tmp"
      pkg_dir=$(find "$tmp" -maxdepth 1 -type d -name "loka-*" | head -1)
      if [ -z "$pkg_dir" ]; then
        pkg_dir="$tmp"
      fi
    else
      warn "Pre-built release not found. Building from source..."
      need_cmd go
      need_cmd git

      git clone --depth 1 https://github.com/vyprai/loka "$tmp/loka-src" 2>/dev/null
      cd "$tmp/loka-src"
      GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/lokad" ./cmd/lokad
      GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/loka-supervisor" ./cmd/loka-supervisor
      GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/loka" ./cmd/loka
      GOOS=$OS GOARCH=$ARCH go build -trimpath -ldflags "-s -w" -o "$tmp/loka-proxy" ./cmd/loka-proxy
      cd - >/dev/null
      pkg_dir="$tmp"
    fi
  fi

  # Install binaries to ~/.loka/bin/ (no sudo needed).
  info "Installing binaries to ${INSTALL_DIR}"
  mkdir -p "$INSTALL_DIR"
  for bin in lokad loka-supervisor loka loka-proxy; do
    if [ -f "$pkg_dir/$bin" ]; then
      install -m 755 "$pkg_dir/$bin" "${INSTALL_DIR}/$bin"
      ok "$bin"
    fi
  done

  # Sign lokad with VZ entitlement on macOS.
  if [ "$OS" = "darwin" ] && [ -f "${INSTALL_DIR}/lokad" ]; then
    local ent_file
    ent_file=$(mktemp)
    cat > "$ent_file" << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>
PLIST
    codesign --entitlements "$ent_file" --force -s - "${INSTALL_DIR}/lokad" 2>/dev/null && ok "lokad signed (VZ entitlement)" || warn "codesign failed"
    rm -f "$ent_file"
  fi

  # Create symlinks in ~/.local/bin/ for PATH access.
  mkdir -p "$SYMLINK_DIR"
  for bin in loka lokad; do
    ln -sf "${INSTALL_DIR}/$bin" "${SYMLINK_DIR}/$bin"
  done
  ok "symlinks → ${SYMLINK_DIR}/"

  # Install VM assets (kernel + initramfs).
  local vm_dir="$HOME/.loka/vm"
  mkdir -p "$vm_dir"
  if [ -d "$pkg_dir/vm" ]; then
    info "Installing VM assets"
    if [ -f "$pkg_dir/vm/vmlinux-lokavm" ]; then
      cp "$pkg_dir/vm/vmlinux-lokavm" "$vm_dir/vmlinux-lokavm"
      ok "kernel → $vm_dir/"
    fi
    if [ -f "$pkg_dir/vm/initramfs.cpio.gz" ]; then
      cp "$pkg_dir/vm/initramfs.cpio.gz" "$vm_dir/initramfs.cpio.gz"
      ok "initramfs → $vm_dir/"
    fi
  else
    warn "Release package does not contain VM assets"
    warn "Build from source: make kernel && make initramfs"
  fi

  # Install Linux supervisor for VM injection (if available).
  if [ -f "$pkg_dir/linux-arm64/loka-supervisor" ]; then
    mkdir -p "${INSTALL_DIR}/linux-arm64"
    install -m 755 "$pkg_dir/linux-arm64/loka-supervisor" "${INSTALL_DIR}/linux-arm64/loka-supervisor"
    ok "linux supervisor → ${INSTALL_DIR}/linux-arm64/"
  elif [ -f "bin/linux-arm64/loka-supervisor" ]; then
    mkdir -p "${INSTALL_DIR}/linux-arm64"
    install -m 755 "bin/linux-arm64/loka-supervisor" "${INSTALL_DIR}/linux-arm64/loka-supervisor"
    ok "linux supervisor → ${INSTALL_DIR}/linux-arm64/"
  fi

  if [ -n "$tmp" ]; then rm -rf "$tmp"; fi
}

# ── Install Cloud Hypervisor (Linux only) ────────────────

install_cloud_hypervisor() {
  if [ "$OS" != "linux" ]; then
    return
  fi

  if command -v cloud-hypervisor &>/dev/null; then
    ok "Cloud Hypervisor already installed"
    return
  fi

  info "Installing Cloud Hypervisor ${CH_VERSION}"

  local ch_arch
  ch_arch=$(uname -m)

  local tmp
  tmp=$(mktemp -d)

  local ch_url="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static-${ch_arch}"

  if curl -fsSL "$ch_url" -o "$tmp/cloud-hypervisor" 2>/dev/null; then
    chmod +x "$tmp/cloud-hypervisor"
    sudo install -m 755 "$tmp/cloud-hypervisor" "/usr/local/bin/cloud-hypervisor"
    ok "cloud-hypervisor → /usr/local/bin/"
  else
    warn "Failed to download Cloud Hypervisor. Install manually:"
    warn "  https://github.com/cloud-hypervisor/cloud-hypervisor/releases"
  fi

  rm -rf "$tmp"
}

# ── Linux dependencies ───────────────────────────────────

install_linux_deps() {
  info "Checking dependencies..."

  # KVM.
  if [ ! -e /dev/kvm ]; then
    warn "/dev/kvm not found — Cloud Hypervisor requires KVM"
    sudo modprobe kvm 2>/dev/null || true
    sudo modprobe kvm_intel 2>/dev/null || sudo modprobe kvm_amd 2>/dev/null || true
    if [ -e /dev/kvm ]; then
      ok "KVM loaded"
    fi
  else
    ok "KVM available"
  fi

  # Ensure current user can access /dev/kvm.
  if [ -e /dev/kvm ] && [ ! -w /dev/kvm ]; then
    info "Adding current user to kvm group..."
    sudo usermod -aG kvm "$(whoami)" 2>/dev/null || true
    sudo chmod 666 /dev/kvm 2>/dev/null || true
    ok "KVM access granted"
  fi
}

# ── Check PATH ───────────────────────────────────────────

check_path() {
  if ! echo "$PATH" | tr ':' '\n' | grep -qx "$SYMLINK_DIR"; then
    echo ""
    warn "${SYMLINK_DIR} is not in your PATH"
    echo -e "  Add to your shell profile:"
    echo -e "    ${DIM}export PATH=\"${SYMLINK_DIR}:\$PATH\"${NC}"
    echo ""
  fi
}

# ── Shell completion ─────────────────────────────────────

install_completions() {
  if ! command -v loka &>/dev/null; then
    return
  fi

  if [ "$OS" = "darwin" ]; then
    local bash_comp_dir
    bash_comp_dir="$(brew --prefix 2>/dev/null)/etc/bash_completion.d" 2>/dev/null || true
    if [ -d "$bash_comp_dir" ]; then
      loka completion bash > "$bash_comp_dir/loka" 2>/dev/null && ok "bash completion"
    fi
    if [ -d /usr/local/share/zsh/site-functions ]; then
      loka completion zsh > /usr/local/share/zsh/site-functions/_loka 2>/dev/null && ok "zsh completion"
    fi
  else
    if [ -d /etc/bash_completion.d ]; then
      loka completion bash | sudo tee /etc/bash_completion.d/loka >/dev/null 2>&1 && ok "bash completion"
    fi
    if [ -d /usr/local/share/zsh/site-functions ]; then
      loka completion zsh | sudo tee /usr/local/share/zsh/site-functions/_loka >/dev/null 2>&1 && ok "zsh completion"
    fi
  fi
}

# ── Main ─────────────────────────────────────────────────

main() {
  detect_platform

  echo ""
  echo -e "${BOLD}  LOKA Installer${NC} — ${PLATFORM}"
  echo ""

  need_cmd curl
  need_cmd tar

  # Stop running lokad before upgrading.
  if pgrep -x lokad &>/dev/null; then
    info "Stopping running lokad..."
    pkill -x lokad 2>/dev/null || true
    sleep 1
    ok "stopped"
  fi

  download_and_install
  echo ""

  case "$OS" in
    linux)
      install_linux_deps
      echo ""
      install_cloud_hypervisor
      ;;
  esac

  install_completions
  check_path

  echo ""
  echo -e "${GREEN}${BOLD}  LOKA installed successfully!${NC}"
  echo ""
  echo -e "  Get started:"
  echo -e "    ${CYAN}loka setup local${NC}           Start LOKA (DNS, proxy, CA trust)"
  echo -e "    ${CYAN}cd myapp && loka deploy${NC}    Deploy your app"
  echo -e "    ${CYAN}loka session create${NC}        Create a session"
  echo -e "    ${CYAN}loka shell${NC}                 Open a shell in a session"
  echo ""
  echo -e "  Observability (built-in, zero-config):"
  echo -e "    ${CYAN}loka metrics${NC}               Metrics TUI (Prometheus-compatible)"
  echo -e "    ${CYAN}loka logs${NC}                  Logs TUI (Loki-compatible)"
  echo -e "    ${CYAN}loka alerts rules${NC}          Manage alert rules"
  echo ""
  echo -e "  Standalone roles:"
  echo -e "    ${CYAN}lokad --role=metrics${NC}        Metrics-only node"
  echo -e "    ${CYAN}lokad --role=logs${NC}           Logs-only node"
  echo -e "    ${CYAN}lokad --role=observability${NC}  Metrics + Logs node"
  echo ""
  echo -e "  ${DIM}Binaries: ${INSTALL_DIR}/${NC}"
  echo -e "  ${DIM}Data:     ~/.loka/${NC}"
  echo -e "  ${DIM}Metrics:  ~/.loka/data/metrics/ (BadgerDB)${NC}"
  echo -e "  ${DIM}Logs:     ~/.loka/data/logs/ (BadgerDB)${NC}"
  echo ""
}

main "$@"
