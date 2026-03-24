#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
#  Build a minimal Lima VM image for LOKA (~90MB ISO)
#
#  Uses alpine-lima "std" edition (lima-init, no cloud-init).
#  LOKA binaries are COPY'd into the mkimage Docker image so
#  genapkovl-lima.sh can include them in the overlay at build time.
#
#  Requires: Docker, Go, git
# ──────────────────────────────────────────────────────────
set -euo pipefail

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  aarch64|arm64) ARCH="aarch64"; GOARCH="arm64" ;;
  x86_64|amd64)  ARCH="x86_64";  GOARCH="amd64" ;;
  *) echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${OUT_DIR:-$PROJECT_DIR/build}"
IMAGE_NAME="loka-lima-${GOARCH}.iso"
ALPINE_VERSION="${ALPINE_VERSION:-3.21.3}"
BUILD_DIR="/tmp/alpine-lima-loka"

echo ""
echo "  Building LOKA Lima image (~90MB)"
echo "  Arch: ${ARCH} | Alpine: ${ALPINE_VERSION}"
echo ""

mkdir -p "$OUT_DIR"

# ── Step 1: Build LOKA binaries ──────────────────────────

echo "==> Building LOKA binaries (linux/${GOARCH})"
cd "$PROJECT_DIR"
GOOS=linux GOARCH=$GOARCH go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/lokad" ./cmd/lokad 2>/dev/null
GOOS=linux GOARCH=$GOARCH go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/loka-worker" ./cmd/loka-worker 2>/dev/null
GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/loka-supervisor" ./cmd/loka-supervisor 2>/dev/null
echo "  Done"

# ── Step 2: Clone alpine-lima ────────────────────────────

echo "==> Preparing alpine-lima"
rm -rf "$BUILD_DIR"
git clone --depth 1 https://github.com/lima-vm/alpine-lima.git "$BUILD_DIR" 2>&1 | tail -1
cd "$BUILD_DIR"
git submodule update --init 2>/dev/null || true

# ── Step 3b: Download Firecracker + kernel ───────────────

FC_VERSION="v1.10.1"
case "$ARCH" in
  aarch64) FC_ARCH="aarch64" ;;
  x86_64)  FC_ARCH="x86_64" ;;
esac

echo "==> Downloading Firecracker ${FC_VERSION} (${FC_ARCH})"
if [ ! -f "$OUT_DIR/firecracker" ]; then
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${FC_ARCH}.tgz" -o "$OUT_DIR/fc.tgz"
  tar -xzf "$OUT_DIR/fc.tgz" -C "$OUT_DIR"
  mv "$OUT_DIR/release-${FC_VERSION}-${FC_ARCH}/firecracker-${FC_VERSION}-${FC_ARCH}" "$OUT_DIR/firecracker"
  rm -rf "$OUT_DIR/release-${FC_VERSION}-${FC_ARCH}" "$OUT_DIR/fc.tgz"
fi
echo "  $(du -h "$OUT_DIR/firecracker" | awk '{print $1}')"

echo "==> Downloading kernel"
if [ ! -f "$OUT_DIR/vmlinux" ]; then
  curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/ci-artifacts/kernels/${FC_ARCH}/vmlinux-5.10.bin" -o "$OUT_DIR/vmlinux"
fi
echo "  $(du -h "$OUT_DIR/vmlinux" | awk '{print $1}')"

# ── Step 3c: Patch overlay to include LOKA ───────────────

echo "==> Patching overlay"

# Insert LOKA additions BEFORE the final tar command in genapkovl-lima.sh.
cat > /tmp/loka-patch.sh << 'LPATCH'
# ── LOKA ─────────────────────────────────────────────────
if [ -d /loka-bins ]; then
  # Binaries.
  mkdir -p "$tmp"/usr/local/bin
  for bin in lokad loka-worker loka-supervisor firecracker; do
    [ -f "/loka-bins/$bin" ] && cp "/loka-bins/$bin" "$tmp/usr/local/bin/$bin" && chmod +x "$tmp/usr/local/bin/$bin"
  done

  # Kernel — store in /usr/share/loka/ (persisted), provision script links it.
  mkdir -p "$tmp"/usr/share/loka
  [ -f "/loka-bins/vmlinux" ] && cp "/loka-bins/vmlinux" "$tmp/usr/share/loka/vmlinux"

  # Data dirs (created but kernel/rootfs linked at boot by provision).
  mkdir -p "$tmp"/var/loka/artifacts "$tmp"/var/loka/tls
fi
LPATCH

python3 -c "
import sys
patch = open('/tmp/loka-patch.sh').read()
lines = open('genapkovl-lima.sh').readlines()
out = []
for l in lines:
    if l.startswith('tar -c -C \"\$tmp\"'):
        out.append(patch)
    out.append(l)
open('genapkovl-lima.sh','w').writelines(out)
"
chmod +x genapkovl-lima.sh

# ── Step 4: Patch Dockerfile to COPY binaries ────────────

# Add all LOKA files into the Docker build image so genapkovl can access them.
cp "$OUT_DIR/lokad" "$BUILD_DIR/lokad"
cp "$OUT_DIR/loka-worker" "$BUILD_DIR/loka-worker"
cp "$OUT_DIR/loka-supervisor" "$BUILD_DIR/loka-supervisor"
cp "$OUT_DIR/firecracker" "$BUILD_DIR/firecracker"
cp "$OUT_DIR/vmlinux" "$BUILD_DIR/vmlinux"

sed -i.bak '/^ENTRYPOINT/i\
COPY lokad loka-worker loka-supervisor firecracker vmlinux /loka-bins/\
RUN chmod +x /loka-bins/*' Dockerfile

echo "  Done"

# ── Step 5: Build mkimage Docker image ───────────────────

echo "==> Building mkimage Docker image (with LOKA binaries)"
make mkimage ALPINE_VERSION="$ALPINE_VERSION" 2>&1 | tail -3

# ── Step 6: Build ISO ────────────────────────────────────

echo "==> Building ISO"

# Set up variables that build.sh needs.
source "edition/std"
REPO_VERSION="v${ALPINE_VERSION%.*}"
BUILD_ID="loka-$(date +%Y%m%d)"
ARCH_ALIAS="$GOARCH"

# Need QEMU COPYING file.
QEMU_VERSION=$(grep "^QEMU_VERSION" Makefile | head -1 | cut -d= -f2 | tr -d ' ')
[ -z "$QEMU_VERSION" ] && QEMU_VERSION="v9.2.2-52"
QEMU_COPYING="qemu-${QEMU_VERSION}-copying"
[ ! -f "$QEMU_COPYING" ] && curl -fsSL "https://raw.githubusercontent.com/qemu/qemu/master/COPYING" -o "$QEMU_COPYING" 2>/dev/null || touch "$QEMU_COPYING"

mkdir -p iso

docker run --rm \
  --platform "linux/${ARCH_ALIAS}" \
  -v "${PWD}/iso:/iso" \
  -v "${PWD}/mkimg.lima.sh:/home/build/aports/scripts/mkimg.lima.sh:ro" \
  -v "${PWD}/genapkovl-lima.sh:/home/build/aports/scripts/genapkovl-lima.sh:ro" \
  -v "${PWD}/lima-init.sh:/home/build/lima-init.sh:ro" \
  -v "${PWD}/lima-init.openrc:/home/build/lima-init.openrc:ro" \
  -v "${PWD}/lima-init-local.openrc:/home/build/lima-init-local.openrc:ro" \
  -v "${PWD}/lima-network.awk:/home/build/lima-network.awk:ro" \
  -v "${PWD}/${QEMU_COPYING}:/home/build/qemu-copying:ro" \
  $(env | grep ^LIMA_ | xargs -n 1 printf -- '-e %s ' 2>/dev/null || true) \
  -e "LIMA_REPO_VERSION=${REPO_VERSION}" \
  -e "LIMA_BUILD_ID=${BUILD_ID}" \
  -e "LIMA_VARIANT_ID=std" \
  "mkimage:${ALPINE_VERSION}-${ARCH}" \
  --tag "std-${ALPINE_VERSION}" \
  --outdir /iso \
  --arch "${ARCH}" \
  --repository "http://dl-cdn.alpinelinux.org/alpine/${REPO_VERSION}/main" \
  --repository "http://dl-cdn.alpinelinux.org/alpine/${REPO_VERSION}/community" \
  --profile lima 2>&1 | tail -3

# ── Step 7: Output ───────────────────────────────────────

ISO_FILE=$(ls -1t iso/alpine-lima-*.iso 2>/dev/null | head -1)
[ -z "$ISO_FILE" ] && { echo "ERROR: ISO not found"; exit 1; }

cp "$ISO_FILE" "$OUT_DIR/${IMAGE_NAME}"
SIZE=$(du -h "$OUT_DIR/${IMAGE_NAME}" | awk '{print $1}')

echo ""
echo "  ${OUT_DIR}/${IMAGE_NAME} (${SIZE})"
echo ""
