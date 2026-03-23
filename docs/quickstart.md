# Quickstart

## Prerequisites

- Linux with KVM (`/dev/kvm`) — or macOS with [Lima](https://lima-vm.io)
- Go 1.24+
- Docker

## Build

```bash
git clone https://github.com/rizqme/loka && cd loka
make build
```

## Setup Firecracker

```bash
make fetch-firecracker   # Firecracker binary + kernel
make build-rootfs        # Guest rootfs with supervisor
```

## Run

```bash
./bin/lokad               # Starts CP with embedded local worker
```

## Use

```bash
# Pull image, create session, run commands
loka image pull ubuntu:22.04
loka session create --image ubuntu:22.04 --name demo
loka exec <session-id> -- echo "Hello from LOKA"
loka exec <session-id> -- python3 -c "print(2+2)"

# Checkpoint and restore
loka checkpoint create <session-id> --label "initial"
loka exec <session-id> -- touch /workspace/newfile
loka checkpoint restore <session-id> <checkpoint-id>
# newfile is gone — restored to checkpoint state

loka session destroy <session-id>
```

## macOS

```bash
make setup-lima && lima bash
# Inside Lima VM: make build-linux && make fetch-firecracker && make build-rootfs
```
