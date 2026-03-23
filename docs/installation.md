# Installation

## One-line install

```bash
curl -fsSL https://rizqme.github.io/loka/install.sh | bash
```

This installs everything: `lokad`, `loka`, `loka-worker`, `loka-supervisor`, Firecracker, and the Linux kernel. It also creates the default config and data directories.

<div class="info"><strong>What it does:</strong> Downloads LOKA binaries, installs Firecracker + kernel, creates <code>/etc/loka/controlplane.yaml</code> and <code>/var/loka/</code>.</div>

### Options

```bash
# Specific version
LOKA_VERSION=v0.1.0 curl -fsSL https://rizqme.github.io/loka/install.sh | bash

# Custom install directory
LOKA_INSTALL_DIR=/opt/loka/bin curl -fsSL https://rizqme.github.io/loka/install.sh | bash

# Custom data directory
LOKA_DATA_DIR=/opt/loka/data curl -fsSL https://rizqme.github.io/loka/install.sh | bash
```

## Verify

```bash
lokad --version
loka version
```

## Start

```bash
lokad                                         # Start the server
loka image pull python:3.12-slim          # Pull an image
loka session create --image python:3.12-slim --mode execute
loka exec <session-id> -- echo "it works"
```

## Install the SDKs

<!-- tabs:start -->

#### **Python**

```bash
pip install loka-sdk
```

#### **TypeScript**

```bash
npm install @rizqme/loka-sdk
```

<!-- tabs:end -->

---

## Prerequisites

| Requirement | Why |
|-------------|-----|
| **Linux with KVM** | Firecracker requires `/dev/kvm` |
| **Docker** | To pull base images (`loka image pull`) |

<div class="tip"><strong>macOS?</strong> The installer puts the binaries on your path, but you need Lima for KVM: <code>curl -fsSL https://rizqme.github.io/loka/install.sh | bash && make setup-lima</code></div>

---

## Alternative: Build from source

```bash
git clone https://github.com/rizqme/loka && cd loka
make build              # Build all 4 binaries
make fetch-firecracker  # Download Firecracker + kernel
make build-rootfs       # Build guest rootfs with supervisor
```

### Binaries

| Binary | What it does |
|--------|-------------|
| `lokad` | Control plane — API server, scheduler, session manager |
| `loka-worker` | Worker agent — manages Firecracker VMs |
| `loka-supervisor` | Runs inside VM as PID 1 — command proxy, sandbox |
| `loka` | CLI client |

## Alternative: Docker

```bash
docker build -f deploy/docker/Dockerfile.controlplane -t loka-cp .
docker build -f deploy/docker/Dockerfile.worker -t loka-worker .
```

## Alternative: Systemd

```bash
sudo cp deploy/systemd/lokad.service /etc/systemd/system/
sudo systemctl enable --now lokad
```

---

## Configuration

### Control Plane

`/etc/loka/controlplane.yaml` (or `LOKA_CONFIG` env var)

```yaml
mode: single              # "single" or "ha"
listen_addr: ":8080"

database:
  driver: sqlite          # "sqlite" or "postgres"
  dsn: "/var/loka/loka.db"

coordinator:
  type: local             # "local" or "redis"

objectstore:
  type: local
  path: "/var/loka/artifacts"

scheduler:
  strategy: spread        # "spread" or "binpack"

auth:
  api_key: ""             # Set to require Bearer token

logging:
  format: text            # "text" or "json"
  level: info
```

### Environment Variables

| Variable | Default |
|----------|---------|
| `LOKA_CONFIG` | `/etc/loka/controlplane.yaml` |
| `LOKA_FIRECRACKER_BIN` | `/usr/local/bin/firecracker` |
| `LOKA_KERNEL_PATH` | `/var/loka/kernel/vmlinux` |
| `LOKA_ROOTFS_PATH` | `/var/loka/rootfs/rootfs.ext4` |

---

## macOS

Firecracker needs Linux + KVM. On macOS, use Lima:

```bash
curl -fsSL https://rizqme.github.io/loka/install.sh | bash    # Install binaries
make setup-lima                            # Create KVM-enabled Linux VM
lima lokad                                 # Run LOKA inside Lima
```
