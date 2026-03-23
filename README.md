# LOKA

LOKA is a controlled execution environment for AI agents. It runs agent-generated code inside Firecracker microVMs where every command, network connection, and file access is governed by policy.

## Install

```bash
curl -fsSL https://rizqme.github.io/loka/install.sh | bash
```

```bash
pip install loka-sdk        # Python
npm install @rizqme/loka-sdk     # TypeScript
```

## How it works

An agent sends a command through the SDK. LOKA starts a Firecracker microVM from a Docker image, runs the command inside it, and returns the result. The VM has a supervisor that enforces access control at the OS level — which binaries can run, which IPs can be reached, which files can be read or written.

```python
from loka import LokaClient

client = LokaClient()
session = client.create_session(image="python:3.12-slim", mode="execute")

result = client.run_and_wait(session.ID, "python3", ["-c", "import pandas; print(pandas.__version__)"])
print(result.Results[0].Stdout)

client.destroy_session(session.ID)
```

## Access control

Sessions have an exec policy that defines what the agent is allowed to do.

```python
session = client.create_session(
    image="ubuntu:22.04",
    mode="ask",
    allowed_commands=["python3", "pip", "git"],
    blocked_commands=["rm", "dd", "nc"],
)
```

**Commands** are controlled by a whitelist and blacklist. Commands not in either list are suspended at an approval gate — the calling system decides whether to allow or deny them, and whether to remember that decision.

```python
ex = client.run(session.ID, "wget", ["http://example.com/data.csv"])
# ex.Status == "pending_approval" — wget is not in the whitelist

client.approve_execution(session.ID, ex.ID, scope="command")  # allow wget for this session
```

Three approval scopes: `once` (this invocation only), `command` (this binary for the session), `always` (persist to the whitelist).

**Network** access is defined per session with rules for IP, CIDR, domain, and port:

```python
session = client.create_session(
    image="python:3.12-slim",
    mode="execute",
    exec_policy={
        "network_policy": {
            "outbound": {
                "default_action": "deny",
                "rules": [
                    {"action": "allow", "target": "*.pypi.org", "ports": "443"},
                    {"action": "allow", "target": "any", "ports": "53", "protocol": "udp"},
                ]
            }
        }
    }
)
```

**Filesystem** access is path-level — which directories are readable, writable, or off-limits:

```python
# In the exec policy:
"filesystem_policy": {
    "default_action": "deny",
    "rules": [
        {"action": "allow", "path": "/workspace/**"},
        {"action": "allow", "path": "/tmp/**"},
        {"action": "deny",  "path": "/workspace/.env"},
    ]
}
```

**Modes** control the overall posture of a session:

| Mode | Filesystem | Network | Approval |
|------|-----------|---------|----------|
| `explore` | Read-only | Blocked | No |
| `execute` | Read/write | Allowed | No |
| `ask` | Read/write | Allowed | Every command |

Modes can be switched at any time: `client.set_mode(session.ID, "ask")`

## Checkpoints

Sessions support checkpointing — capturing the filesystem diff from the base image. Checkpoints form a DAG, so the agent can branch execution and roll back to any prior state.

```python
cp = client.create_checkpoint(session.ID, label="before-experiment")

client.run_and_wait(session.ID, "pip", ["install", "some-package"])
# Something went wrong...

client.restore_checkpoint(session.ID, cp.ID)
# Filesystem is back to the checkpoint state
```

## Images

Sessions start from Docker images. LOKA pulls the image, converts it to a Firecracker rootfs, and creates a warm snapshot. Subsequent sessions restore from the snapshot in ~28ms instead of booting from scratch.

```python
client.pull_image("python:3.12-slim")  # Once — pulls, converts, warms
session = client.create_session(image="python:3.12-slim")  # ~28ms
```

## Streaming

Command output can be streamed in real-time via SSE:

```python
for event in client.stream(session.ID, "python3", ["train.py"]):
    if event.is_output:
        print(event.text, end="")
    if event.is_done:
        break
```

## Architecture

LOKA has a control plane (`lokad`) that manages sessions and schedules them onto workers. Each worker runs Firecracker VMs. Inside each VM, a supervisor process handles command execution and policy enforcement via vsock.

The control plane supports SQLite for development and PostgreSQL + Raft for production HA deployments. Workers can run on AWS, GCP, Azure, OVH, DigitalOcean, local machines, or self-managed servers.

```
Agent → SDK → Control Plane → Worker → Firecracker VM → Supervisor → Process
                                              │
                                    Command proxy (binary gate)
                                    Network filter (iptables)
                                    Filesystem guard (landlock)
                                    Seccomp (syscall filter)
```

## Documentation

[docs.loka.dev](https://docs.loka.dev)

## License

Apache 2.0
