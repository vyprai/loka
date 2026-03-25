#!/usr/bin/env bash
# Create and start a Lima VM for LOKA. Used by `make install`.
set -euo pipefail

LIMA_LOG="/tmp/lima-make-install.log"
IMG_BASE="https://github.com/vyprai/loka/releases/latest/download"
CFG=$(mktemp)

# Detect image: custom ISO or Alpine cloud fallback.
if curl -fsSL -o /dev/null -w '%{http_code}' "${IMG_BASE}/loka-lima-arm64.iso" 2>/dev/null | grep -q "200"; then
  echo "  Using pre-built LOKA ISO"
  cat > "$CFG" <<YAML
vmType: vz
nestedVirtualization: true
containerd:
  system: false
  user: false
images:
  - location: "${IMG_BASE}/loka-lima-arm64.iso"
    arch: "aarch64"
  - location: "${IMG_BASE}/loka-lima-amd64.iso"
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
  - guestPort: 6843
    hostPort: 6843
  - guestPort: 5453
    hostPort: 5453
    proto: udp
provision:
  - mode: system
    script: |
      #!/bin/sh
      set -eux
      [ -e /dev/kvm ] && chmod 666 /dev/kvm || true
      mkdir -p /var/loka/kernel /tmp/loka-data/kernel /tmp/loka-data/rootfs /tmp/loka-data/objstore
      [ -f /usr/share/loka/vmlinux ] && cp /usr/share/loka/vmlinux /var/loka/kernel/vmlinux
      [ -f /var/loka/kernel/vmlinux ] && ln -sf /var/loka/kernel/vmlinux /tmp/loka-data/kernel/vmlinux
      [ -f /usr/share/loka/rootfs.ext4 ] && cp /usr/share/loka/rootfs.ext4 /tmp/loka-data/rootfs/rootfs.ext4
YAML
else
  echo "  Using Alpine cloud image"
  cat > "$CFG" <<'YAML'
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
  - guestPort: 6843
    hostPort: 6843
  - guestPort: 5453
    hostPort: 5453
    proto: udp
provision:
  - mode: system
    script: |
      #!/bin/sh
      set -eux
      apk add --no-cache curl iptables iproute2 e2fsprogs docker >/dev/null 2>&1
      rc-update add docker default 2>/dev/null || true
      service docker start 2>/dev/null || true
      [ -e /dev/kvm ] && chmod 666 /dev/kvm || true
YAML
fi

echo -n "  Creating VM..."
limactl create --name=loka --tty=false "$CFG" > "$LIMA_LOG" 2>&1
rm -f "$CFG"
echo " done"

echo -n "  Starting VM (may take a minute)..."
if limactl start loka >> "$LIMA_LOG" 2>&1; then
  echo " ready!"
  echo "  ✓ Lima VM running"
else
  echo " failed"
  echo "  Check: $LIMA_LOG"
  tail -5 "$LIMA_LOG"
  exit 1
fi
