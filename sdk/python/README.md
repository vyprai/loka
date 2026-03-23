# loka-sdk

Python SDK for LOKA.

## Install

```bash
pip install loka-sdk
```

## Usage

```python
from loka import LokaClient

client = LokaClient("http://localhost:6840")

# Pull image and create session
client.pull_image("python:3.12-slim")
session = client.create_session(image="python:3.12-slim", mode="execute")

# Run commands
result = client.run_and_wait(session.ID, "python3", ["-c", "print(42)"])
print(result.Results[0].Stdout)  # "42\n"

# Checkpoint and restore
cp = client.create_checkpoint(session.ID, label="initial")
client.restore_checkpoint(session.ID, cp.ID)

# Cleanup
client.destroy_session(session.ID)
```

## Approval Flow

```python
session = client.create_session(image="ubuntu:22.04", mode="ask")

ex = client.run(session.ID, "wget", ["http://example.com"])
if ex.Status == "pending_approval":
    client.approve_execution(session.ID, ex.ID, add_to_whitelist=True)
    ex = client.wait_for_execution(session.ID, ex.ID)
```

## Context Manager

```python
with LokaClient("http://localhost:6840") as client:
    session = client.create_session(image="ubuntu:22.04")
    # ...
```
