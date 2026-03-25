#!/usr/bin/env bash
# Build an elastic Alpine rootfs for the LOKA VZ VM.
# Output: build/loka-rootfs.ext4 (sparse, 50GB virtual, ~20MB actual)
#
# The rootfs includes:
#   - Alpine base system with openrc
#   - loka-vmagent: in-VM exec agent (auto-started on boot via vsock:2222)
#   - loka-supervisor: session sandbox process
#   - lokad: control plane / worker daemon
#   - fstrim cron for elastic disk reclaim
#
# Usage:
#   bash scripts/build-rootfs.sh                    # default output
#   bash scripts/build-rootfs.sh build/custom.ext4  # custom output path
#
# Requires: Docker (or podman), mkfs.ext4, sudo (for mount/umount)

set -euo pipefail

OUT="${1:-build/loka-rootfs.ext4}"
ARCH=$(uname -m)
[ "$ARCH" = "arm64" ] && ARCH="aarch64"
[ "$ARCH" = "x86_64" ] && true  # already correct

ALPINE_VERSION="3.21"
ALPINE_RELEASE="3.21.3"

mkdir -p "$(dirname "$OUT")"

# ── Step 1: Build Linux binaries via Docker ──────────────

echo "==> Building Linux binaries"

# Determine Go arch for cross-compilation.
case "$ARCH" in
  aarch64) GOARCH=arm64 ;;
  x86_64)  GOARCH=amd64 ;;
  *)       echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

BIN_DIR="bin/linux-${GOARCH}"
mkdir -p "$BIN_DIR"

# Cross-compile if binaries don't exist.
for bin in lokad loka-worker loka-supervisor loka-vmagent; do
  if [ ! -f "$BIN_DIR/$bin" ]; then
    echo "    Building $bin for linux/$GOARCH..."
    GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags "-s -w" -o "$BIN_DIR/$bin" "./cmd/$bin"
  fi
done

# ── Step 2: Create elastic sparse ext4 image ────────────

echo "==> Creating elastic rootfs (50GB virtual, sparse)"
rm -f "$OUT"
truncate -s 50G "$OUT"
mkfs.ext4 -F -q "$OUT"

# ── Step 3: Populate via Docker (no host root needed for this part) ──

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/Dockerfile" <<DOCKER
FROM alpine:${ALPINE_VERSION}

# Install essential packages.
RUN apk add --no-cache \
    openrc busybox-initscripts e2fsprogs iproute2 iptables \
    util-linux-misc coreutils bash python3 git curl \
    && rm -rf /var/cache/apk/*

# Create loka directories.
RUN mkdir -p /usr/local/bin /env/bin /workspace /tmp /var/loka /tmp/loka-data

# Enable essential services.
RUN rc-update add devfs sysinit && \
    rc-update add procfs boot && \
    rc-update add sysfs boot

# Network setup.
RUN printf 'auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet dhcp\n' > /etc/network/interfaces

# fstab with discard for elastic disk.
RUN echo '/dev/vda / ext4 defaults,discard 0 1' > /etc/fstab

# fstrim cron for elastic disk reclaim.
RUN mkdir -p /etc/crontabs && echo '*/5 * * * * fstrim / 2>/dev/null' > /etc/crontabs/root

# Set PATH.
ENV PATH="/env/bin:/usr/local/bin:/usr/bin:/bin"
DOCKER

echo "    Building Docker image..."
docker build -t loka-rootfs-builder "$TMP" -q

echo "    Extracting rootfs..."
CONTAINER_ID=$(docker create loka-rootfs-builder)
docker export "$CONTAINER_ID" > "$TMP/rootfs.tar"
docker rm "$CONTAINER_ID" >/dev/null

# ── Step 4: Mount and populate image ────────────────────

echo "==> Populating rootfs image"
MOUNT_DIR="$TMP/mount"
mkdir -p "$MOUNT_DIR"

if command -v guestmount &>/dev/null; then
  # Use libguestfs if available (no root needed).
  guestmount -a "$OUT" -i "$MOUNT_DIR"
  tar -xf "$TMP/rootfs.tar" -C "$MOUNT_DIR"

  # Inject LOKA binaries.
  for bin in lokad loka-worker loka-supervisor loka-vmagent; do
    [ -f "$BIN_DIR/$bin" ] && cp "$BIN_DIR/$bin" "$MOUNT_DIR/usr/local/bin/$bin"
  done
  chmod +x "$MOUNT_DIR/usr/local/bin/"* 2>/dev/null || true

  # Create vmagent init script.
  cat > "$MOUNT_DIR/etc/init.d/loka-vmagent" <<'INITSCRIPT'
#!/sbin/openrc-run
description="LOKA VM Agent"
command="/usr/local/bin/loka-vmagent"
command_background=true
pidfile="/run/loka-vmagent.pid"
output_log="/var/log/loka-vmagent.log"
error_log="/var/log/loka-vmagent.log"
INITSCRIPT
  chmod +x "$MOUNT_DIR/etc/init.d/loka-vmagent"

  # Enable vmagent on boot (write symlink directly since we can't chroot in guestmount).
  mkdir -p "$MOUNT_DIR/etc/runlevels/default"
  ln -sf /etc/init.d/loka-vmagent "$MOUNT_DIR/etc/runlevels/default/loka-vmagent"

  guestunmount "$MOUNT_DIR"
else
  # Fallback: use sudo mount (needs root).
  sudo mount -o loop "$OUT" "$MOUNT_DIR"
  sudo tar -xf "$TMP/rootfs.tar" -C "$MOUNT_DIR"

  # Inject LOKA binaries.
  for bin in lokad loka-worker loka-supervisor loka-vmagent; do
    [ -f "$BIN_DIR/$bin" ] && sudo cp "$BIN_DIR/$bin" "$MOUNT_DIR/usr/local/bin/$bin"
  done
  sudo chmod +x "$MOUNT_DIR/usr/local/bin/"* 2>/dev/null || true

  # Create vmagent init script.
  cat <<'INITSCRIPT' | sudo tee "$MOUNT_DIR/etc/init.d/loka-vmagent" >/dev/null
#!/sbin/openrc-run
description="LOKA VM Agent"
command="/usr/local/bin/loka-vmagent"
command_background=true
pidfile="/run/loka-vmagent.pid"
output_log="/var/log/loka-vmagent.log"
error_log="/var/log/loka-vmagent.log"
INITSCRIPT
  sudo chmod +x "$MOUNT_DIR/etc/init.d/loka-vmagent"
  sudo chroot "$MOUNT_DIR" rc-update add loka-vmagent default

  sudo umount "$MOUNT_DIR"
fi

ACTUAL=$(du -h "$OUT" | awk '{print $1}')
echo "==> Done: $OUT (50GB virtual, ${ACTUAL} actual)"
