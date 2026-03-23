"""LOKA SDK type definitions."""

from __future__ import annotations
from dataclasses import dataclass, field
from typing import Any


@dataclass
class PortMapping:
    """Port forwarding from local machine to session VM."""
    local_port: int = 0
    remote_port: int = 0
    protocol: str = "tcp"


@dataclass
class StorageMount:
    """Object storage bucket mounted into a session VM."""
    provider: str = ""         # "s3", "gcs", "azure-blob", "local"
    bucket: str = ""
    mount_path: str = ""
    prefix: str = ""
    read_only: bool = False
    region: str = ""
    endpoint: str = ""         # For S3-compatible (MinIO, R2)
    credentials: dict[str, str] = field(default_factory=dict)

    @staticmethod
    def s3(bucket: str, mount_path: str, *, access_key_id: str = "", secret_access_key: str = "",
           prefix: str = "", read_only: bool = False, region: str = "", endpoint: str = "") -> "StorageMount":
        """Create an S3 mount."""
        creds = {}
        if access_key_id:
            creds["access_key_id"] = access_key_id
        if secret_access_key:
            creds["secret_access_key"] = secret_access_key
        return StorageMount(provider="s3", bucket=bucket, mount_path=mount_path,
                            prefix=prefix, read_only=read_only, region=region,
                            endpoint=endpoint, credentials=creds)

    @staticmethod
    def gcs(bucket: str, mount_path: str, *, service_account_json: str = "",
            prefix: str = "", read_only: bool = False) -> "StorageMount":
        """Create a GCS mount."""
        creds = {}
        if service_account_json:
            creds["service_account_json"] = service_account_json
        return StorageMount(provider="gcs", bucket=bucket, mount_path=mount_path,
                            prefix=prefix, read_only=read_only, credentials=creds)

    @staticmethod
    def azure(container: str, mount_path: str, *, account_name: str = "", account_key: str = "",
              sas_token: str = "", prefix: str = "", read_only: bool = False) -> "StorageMount":
        """Create an Azure Blob mount."""
        creds = {}
        if account_name:
            creds["account_name"] = account_name
        if account_key:
            creds["account_key"] = account_key
        if sas_token:
            creds["sas_token"] = sas_token
        return StorageMount(provider="azure-blob", bucket=container, mount_path=mount_path,
                            prefix=prefix, read_only=read_only, credentials=creds)


@dataclass
class Session:
    ID: str = ""
    Name: str = ""
    Status: str = ""
    Mode: str = ""
    WorkerID: str = ""
    ImageRef: str = ""
    ImageID: str = ""
    SnapshotID: str = ""
    VCPUs: int = 0
    MemoryMB: int = 0
    Labels: dict[str, str] = field(default_factory=dict)
    Mounts: list[Any] = field(default_factory=list)
    Ports: list[Any] = field(default_factory=list)
    Ready: bool = False
    StatusMessage: str = ""
    CreatedAt: str = ""
    UpdatedAt: str = ""


@dataclass
class CommandResult:
    CommandID: str = ""
    ExitCode: int = 0
    Stdout: str = ""
    Stderr: str = ""
    StartedAt: str = ""
    EndedAt: str = ""


@dataclass
class Execution:
    ID: str = ""
    SessionID: str = ""
    Status: str = ""
    Parallel: bool = False
    Commands: list[dict[str, Any]] = field(default_factory=list)
    Results: list[CommandResult] = field(default_factory=list)
    CreatedAt: str = ""
    UpdatedAt: str = ""


@dataclass
class Checkpoint:
    ID: str = ""
    SessionID: str = ""
    ParentID: str = ""
    Type: str = ""
    Status: str = ""
    Label: str = ""
    CreatedAt: str = ""


@dataclass
class Image:
    ID: str = ""
    Reference: str = ""
    Digest: str = ""
    SizeMB: int = 0
    Status: str = ""
    CreatedAt: str = ""


@dataclass
class StreamEvent:
    """A single event from a streaming execution."""
    event: str = ""   # output, status, result, approval_required, error, done
    data: dict = field(default_factory=dict)

    @property
    def is_output(self) -> bool: return self.event == "output"
    @property
    def is_done(self) -> bool: return self.event == "done"
    @property
    def text(self) -> str: return self.data.get("text", "")
    @property
    def stream_name(self) -> str: return self.data.get("stream", "")


@dataclass
class Worker:
    ID: str = ""
    Hostname: str = ""
    Provider: str = ""
    Region: str = ""
    Status: str = ""
    Labels: dict[str, str] = field(default_factory=dict)
