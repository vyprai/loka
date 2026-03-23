# LOKA

Controlled execution environment for AI agents. Runs code inside Firecracker microVMs where every command, network connection, and file access is governed by policy.

**[Documentation](https://vyprai.github.io/loka)**

## Install

```bash
curl -fsSL https://vyprai.github.io/loka/install.sh | bash
```

Works on Linux (native) and macOS (auto-creates a Lima VM with KVM). TLS is enabled by default.

## Quick start

```bash
loka deploy local                              # Start the server
loka image pull python:3.12-slim               # Pull a Docker image
loka session create --image python:3.12-slim   # Create a session
loka exec <id> -- python3 -c "print('hello from LOKA')"
loka shell <id>                                # Interactive shell
loka session artifacts <id>                    # See what files changed
loka session download <id> /workspace/out.csv  # Download a result
loka deploy down                               # Stop
```

## SDKs

```bash
pip install loka-sdk                           # Python
npm install @vypr-ai/loka-sdk                  # TypeScript
```

```python
from loka import LokaClient

client = LokaClient()
session = client.create_session(image="python:3.12-slim", mode="execute")
result = client.run_and_wait(session.ID, "python3", ["-c", "print(42)"])
print(result.Results[0].Stdout)

# Get artifacts — files the agent created
artifacts = client.list_artifacts(session.ID)
data = client.download_artifact(session.ID, "/workspace/output.csv")

client.destroy_session(session.ID)
```

## Deploy

```bash
# Local (dev)
loka deploy local

# Cloud
loka deploy aws --name prod --region us-east-1 --workers 3
loka deploy gcp --name staging --project my-proj --workers 2
loka deploy azure --name eu --region westeurope --workers 2
loka deploy do --name nyc --region nyc1 --workers 2
loka deploy ovh --name fra --region gra --workers 2

# Your own VMs (SSH-based)
loka deploy vm --name prod --cp 10.0.0.1 --worker 10.0.0.2 --worker 10.0.0.3

# Declarative (YAML)
loka deploy apply cluster.yml

# Manage
loka list                                      # List servers
loka use prod                                  # Switch active server
loka current                                   # Show active server
loka worker add 10.0.0.5                       # Add a worker
loka worker remove 10.0.0.5                    # Remove a worker
loka worker scale 5                            # Scale (cloud providers)
loka deploy export prod > prod.yml             # Export as YAML
```

## Sessions

```python
session = client.create_session(image="python:3.12-slim")  # Blocks until ready

# Interactive shell
# loka shell <id>

# Port forwarding
session = client.create_session(
    image="python:3.12-slim",
    ports=[{"local_port": 8080, "remote_port": 5000}]
)

# Storage mounts
from loka import StorageMount
session = client.create_session(
    image="python:3.12-slim",
    mounts=[StorageMount.s3("my-bucket", "/data", access_key_id="...", secret_access_key="...")]
)

# Idle / auto-wake
client.idle_session(session.ID)                # Suspend to save resources
client.run(session.ID, "python3", ["script.py"])  # Auto-wakes

# Domain exposure
client.expose_session(session.ID, "my-app", 5000)
# Access at: https://my-app.loka.example.com
```

## Access control

Sessions have an exec policy that defines what the agent is allowed to do.

**Commands** are controlled by a whitelist and blacklist. Unknown commands are suspended at an approval gate — the calling system decides whether to allow or deny.

```python
session = client.create_session(
    image="ubuntu:22.04",
    mode="ask",
    allowed_commands=["python3", "pip", "git"],
    blocked_commands=["rm", "dd", "nc"],
)

ex = client.run(session.ID, "wget", ["http://example.com/data.csv"])
# ex.Status == "pending_approval"
client.approve_execution(session.ID, ex.ID, scope="command")
```

**Modes** control the overall posture:

| Mode | Filesystem | Network | Approval |
|------|-----------|---------|----------|
| `explore` | Read-only | Blocked | No |
| `execute` | Read/write | Allowed | No |
| `ask` | Read/write | Allowed | Every command |

## Checkpoints & Artifacts

Capture filesystem diffs. Checkpoints form a DAG — branch execution and roll back to any prior state.

```python
cp = client.create_checkpoint(session.ID, label="before-experiment")
client.run_and_wait(session.ID, "pip", ["install", "some-package"])
# Something went wrong...
client.restore_checkpoint(session.ID, cp.ID)

# See what changed
artifacts = client.list_artifacts(session.ID)
artifacts = client.list_artifacts(session.ID, checkpoint_id=cp.ID)

# Download results
data = client.download_artifact(session.ID, "/workspace/output.csv")
client.download_artifacts(session.ID, "./output/")  # All as tar
```

## Architecture

```
Agent → SDK → Control Plane → Worker → Firecracker VM → Supervisor → Process
                                              │
                                    Command proxy (binary gate)
                                    Network filter (iptables)
                                    Filesystem guard (landlock)
                                    Seccomp (syscall filter)
```

- **Control plane** (`lokad`) — API server, scheduler, session manager
- **Worker** (`loka-worker`) — manages Firecracker VMs
- **Supervisor** (`loka-supervisor`) — runs inside VM as PID 1, enforces policy
- **CLI** (`loka`) — deploy, manage, interact

SQLite for dev, PostgreSQL + embedded Raft for production HA. Workers on AWS, GCP, Azure, OVH, DigitalOcean, VMs, or self-managed. Auto-TLS on all connections.

## Documentation

**[vyprai.github.io/loka](https://vyprai.github.io/loka)**

## License

Apache 2.0
