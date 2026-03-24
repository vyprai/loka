#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────
#  Build a custom Lima VM image for LOKA
#
#  Takes the Alpine cloud image, injects LOKA binaries and
#  pre-configures everything so no provision step is needed.
#  Result: a bootable qcow2 ready for Lima.
#
#  Usage:
#    bash scripts/build-lima-image.sh
#    ARCH=amd64 bash scripts/build-lima-image.sh
#
#  Requires: Docker
# ──────────────────────────────────────────────────────────
set -euo pipefail

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in
  aarch64|arm64) ARCH="aarch64"; GOARCH="arm64"; PLATFORM="linux/arm64" ;;
  x86_64|amd64)  ARCH="x86_64";  GOARCH="amd64"; PLATFORM="linux/amd64" ;;
  *) echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

OUT_DIR="${OUT_DIR:-./build}"
IMAGE_NAME="loka-lima-${GOARCH}.qcow2"
ALPINE_IMG="nocloud_alpine-3.23.3-${ARCH}-uefi-cloudinit-r0.qcow2"
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.23/releases/cloud/${ALPINE_IMG}"

echo ""
echo "  Building LOKA Lima image"
echo "  Arch: ${ARCH} (${GOARCH})"
echo "  Base: ${ALPINE_IMG}"
echo "  Output: ${OUT_DIR}/${IMAGE_NAME}"
echo ""

mkdir -p "$OUT_DIR"

# ── Build LOKA binaries for Linux ────────────────────────

echo "==> Building LOKA binaries (linux/${GOARCH})"
GOOS=linux GOARCH=$GOARCH go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/lokad" ./cmd/lokad 2>/dev/null
GOOS=linux GOARCH=$GOARCH go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/loka-worker" ./cmd/loka-worker 2>/dev/null
GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/loka-supervisor" ./cmd/loka-supervisor 2>/dev/null
echo "  Done"

# ── Customize Alpine cloud image in Docker ───────────────

echo "==> Customizing Alpine cloud image in Docker"

cat > "$OUT_DIR/customize.sh" << 'SCRIPT'
#!/bin/sh
set -eux

ARCH="$1"
ALPINE_URL="$2"

apk add --no-cache qemu-img e2fsprogs e2fsprogs-extra curl parted sgdisk >/dev/null 2>&1

cd /work

# Download Alpine cloud image.
echo "Downloading Alpine cloud image..."
curl -fsSL "$ALPINE_URL" -o base.qcow2

# Convert to raw and expand for extra space.
qemu-img convert -f qcow2 -O raw base.qcow2 disk.raw
rm base.qcow2
qemu-img resize -f raw disk.raw 2G

# Fix GPT after resize and expand root partition to fill disk.
# Alpine cloud images: partition 1 = EFI, partition 2 = root.
sgdisk -e disk.raw
parted -s disk.raw resizepart 2 100%

# Find the root partition offset and resize filesystem.
ROOT_START=$(parted -s disk.raw unit B print | awk '/^ 2/{print $2}' | tr -d 'B')
ROOT_SIZE=$(parted -s disk.raw unit B print | awk '/^ 2/{print $4}' | tr -d 'B')
if [ -z "$ROOT_START" ]; then
  echo "ERROR: cannot find root partition"; exit 1
fi
echo "Root partition at offset $ROOT_START, size $ROOT_SIZE"

# Resize the filesystem to fill the expanded partition.
# Use a temp loop device for e2fsck/resize2fs.
LOOP=$(losetup -f)
losetup -o "$ROOT_START" "$LOOP" disk.raw
e2fsck -fy "$LOOP" || true
resize2fs "$LOOP"
losetup -d "$LOOP"

# Mount the root partition.
mkdir -p /mnt/rootfs
mount -o loop,offset=$ROOT_START disk.raw /mnt/rootfs

# ── Install LOKA binaries ────────────────────────────────
cp /work/lokad /mnt/rootfs/usr/local/bin/lokad
cp /work/loka-worker /mnt/rootfs/usr/local/bin/loka-worker
cp /work/loka-supervisor /mnt/rootfs/usr/local/bin/loka-supervisor
chmod +x /mnt/rootfs/usr/local/bin/lokad /mnt/rootfs/usr/local/bin/loka-worker /mnt/rootfs/usr/local/bin/loka-supervisor

# ── Install packages via chroot ──────────────────────────
cp /etc/resolv.conf /mnt/rootfs/etc/resolv.conf
mount -t proc proc /mnt/rootfs/proc
mount -t sysfs sys /mnt/rootfs/sys
mount --bind /dev /mnt/rootfs/dev

chroot /mnt/rootfs /bin/sh -c '
  apk update >/dev/null 2>&1
  apk add --no-cache docker iptables iproute2 e2fsprogs curl >/dev/null 2>&1

  # Enable Docker and KVM.
  rc-update add docker default 2>/dev/null || true
  echo "kvm" >> /etc/modules

  # Create LOKA data directories.
  mkdir -p /var/loka/kernel /var/loka/artifacts /var/loka/worker /var/loka/raft /var/loka/tls
  mkdir -p /tmp/loka-data/kernel /tmp/loka-data/rootfs /tmp/loka-data/objstore /tmp/loka-data/worker-data

  # Clean caches.
  rm -rf /var/cache/apk/*
'

umount /mnt/rootfs/dev /mnt/rootfs/proc /mnt/rootfs/sys
umount /mnt/rootfs

# ── Recompress to qcow2 ─────────────────────────────────
qemu-img convert -f raw -O qcow2 -c disk.raw /work/output.qcow2
rm disk.raw

echo "==> Image size: $(du -h /work/output.qcow2 | awk '{print $1}')"
SCRIPT

chmod +x "$OUT_DIR/customize.sh"

docker run --rm --privileged \
  --platform "$PLATFORM" \
  -v "$(cd "$OUT_DIR" && pwd):/work" \
  alpine:3.21 \
  /work/customize.sh "$ARCH" "$ALPINE_URL"

mv "$OUT_DIR/output.qcow2" "$OUT_DIR/${IMAGE_NAME}"
rm -f "$OUT_DIR/customize.sh" "$OUT_DIR/lokad" "$OUT_DIR/loka-worker" "$OUT_DIR/loka-supervisor"

SIZE=$(du -h "$OUT_DIR/${IMAGE_NAME}" | awk '{print $1}')
echo ""
echo "  Image built: ${OUT_DIR}/${IMAGE_NAME} (${SIZE})"
echo ""
echo "  Upload to GitHub release:"
echo "    gh release upload v\$(cat pkg/version/version.go | grep Version | grep -o 'v[0-9.]*') ${OUT_DIR}/${IMAGE_NAME}"
echo ""
