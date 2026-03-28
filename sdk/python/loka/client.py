"""LOKA Python SDK client."""

from __future__ import annotations

import json
import time
from typing import Any, Generator

import httpx

from loka.types import Session, Execution, CommandResult, Checkpoint, Image, Worker, StreamEvent, Artifact, Service, ServiceRoute, VolumeRecord, WorkerToken, ObjectInfo


class LokaClient:
    """Client for the LOKA control plane API."""

    def __init__(
        self,
        base_url: str = "http://localhost:6840",
        token: str = "",
        timeout: float = 30.0,
    ):
        headers = {}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        self._http = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
        )

    def close(self):
        self._http.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()

    # ── Sessions ─────────────────────────────────────────

    def create_session(self, wait: bool = True, timeout: float = 120, **kwargs) -> Session:
        """Create a new session.

        Blocks until the session is ready by default. Set wait=False to return immediately.

        Args:
            wait: Wait for session to be ready (default True).
            timeout: Max seconds to wait (default 120).
            name: Session name.
            image: Docker image reference (e.g., "python:3.12-slim").
            snapshot_id: Optional snapshot ID to restore from.
            mode: Execution mode (explore, execute, ask).
            vcpus: Number of vCPUs.
            memory_mb: Memory in MB.
            allowed_commands: Command whitelist.
            blocked_commands: Command blacklist.
            mounts: List of storage mounts.
            ports: List of port mappings.
        """
        import time as _time
        sess = self._as(Session, self._post("/api/v1/sessions", kwargs))
        if not wait or sess.Ready or sess.Status == "running":
            return sess
        deadline = _time.monotonic() + timeout
        while not sess.Ready and sess.Status not in ("running", "error"):
            if _time.monotonic() > deadline:
                raise TimeoutError(f"session {sess.ID} not ready after {timeout}s (status: {sess.Status})")
            _time.sleep(0.5)
            sess = self.get_session(sess.ID)
        if sess.Status == "error":
            raise RuntimeError(f"session failed: {sess.StatusMessage or 'unknown error'}")
        return sess

    def get_session(self, session_id: str) -> Session:
        return self._as(Session, self._get(f"/api/v1/sessions/{session_id}"))

    def list_sessions(self, status: str = "") -> list[Session]:
        q = f"?status={status}" if status else ""
        data = self._get(f"/api/v1/sessions{q}")
        return [self._as(Session, s) for s in data.get("sessions", [])]

    def destroy_session(self, session_id: str) -> None:
        self._delete(f"/api/v1/sessions/{session_id}")

    def sync_mount(self, session_id: str, mount_path: str, direction: str = "push",
                   prefix: str = "", delete: bool = False, dry_run: bool = False) -> dict:
        """Sync data between a session's storage mount and the object store.

        Args:
            session_id: Session ID.
            mount_path: The mount path to sync (e.g. "/data").
            direction: "push" (VM → bucket) or "pull" (bucket → VM).
            prefix: Limit sync to a sub-path within the mount.
            delete: Delete files in destination not in source.
            dry_run: Preview changes without syncing.
        """
        return self._post(f"/api/v1/sessions/{session_id}/sync", {
            "mount_path": mount_path,
            "direction": direction,
            "prefix": prefix,
            "delete": delete,
            "dry_run": dry_run,
        })

    def port_forward(self, session_id: str, local_port: int, remote_port: int) -> None:
        """Forward a local port to a port inside the session VM.

        Opens a local TCP listener and tunnels connections via gRPC streaming.
        This is a blocking call — runs until interrupted.

        Args:
            session_id: Session ID.
            local_port: Port on your machine.
            remote_port: Port inside the VM.

        Note: Requires gRPC streaming. Use the CLI:
            loka session port-forward <id> <local>:<remote>
        """
        raise NotImplementedError(
            "port_forward requires gRPC streaming. Use the CLI: "
            f"loka session port-forward {session_id} {local_port}:{remote_port}"
        )

    def mount_local(self, session_id: str, local_path: str, vm_path: str,
                    read_only: bool = False) -> None:
        """Mount a local directory into a session via gRPC tunnel.

        This is a blocking call — it keeps the tunnel open until interrupted.
        The local directory is served on-demand to the VM over gRPC streaming.

        Args:
            session_id: Session ID.
            local_path: Local directory path on your machine.
            vm_path: Where it appears inside the VM (e.g. "/workspace").
            read_only: Mount as read-only.

        Note: Requires gRPC connection. Use LokaClient(url="grpc://host:6841").
        """
        raise NotImplementedError(
            "mount_local requires gRPC streaming. Use the CLI: "
            "loka session mount <id> <local-path> <vm-path>"
        )

    def idle_session(self, session_id: str) -> Session:
        return self._as(Session, self._post(f"/api/v1/sessions/{session_id}/idle"))

    def pause_session(self, session_id: str) -> Session:
        return self._as(Session, self._post(f"/api/v1/sessions/{session_id}/pause"))

    def resume_session(self, session_id: str) -> Session:
        return self._as(Session, self._post(f"/api/v1/sessions/{session_id}/resume"))

    def set_mode(self, session_id: str, mode: str) -> Session:
        return self._as(Session, self._post(f"/api/v1/sessions/{session_id}/mode", {"mode": mode}))

    def expose_session(self, session_id: str, domain: str, remote_port: int) -> dict:
        return self._post(f"/api/v1/sessions/{session_id}/expose", {
            "domain": domain,
            "remote_port": remote_port,
        })

    def unexpose_session(self, session_id: str, domain: str) -> None:
        self._delete(f"/api/v1/sessions/{session_id}/expose/{domain}")

    def wait_until_ready(self, session_id: str, timeout: float = 120) -> Session:
        """Poll until session is ready or errors out.

        Args:
            session_id: Session ID.
            timeout: Max seconds to wait (default 120).
        """
        import time as _time
        deadline = _time.monotonic() + timeout
        sess = self.get_session(session_id)
        while not sess.Ready and sess.Status not in ("running", "error"):
            if _time.monotonic() > deadline:
                raise TimeoutError(f"session {session_id} not ready after {timeout}s (status: {sess.Status})")
            _time.sleep(0.5)
            sess = self.get_session(session_id)
        if sess.Status == "error":
            raise RuntimeError(f"session failed: {sess.StatusMessage or 'unknown error'}")
        return sess

    # ── Command Execution ────────────────────────────────

    def run(self, session_id: str, command: str, args: list[str] | None = None, **kwargs) -> Execution:
        """Run a single command in a session.

        Args:
            session_id: Session ID.
            command: Binary to run.
            args: Command arguments.
            workdir: Working directory.
            env: Environment variables.
        """
        body: dict[str, Any] = {"command": command}
        if args:
            body["args"] = args
        body.update(kwargs)
        return self._as_execution(self._post(f"/api/v1/sessions/{session_id}/exec", body))

    def run_parallel(self, session_id: str, commands: list[dict]) -> Execution:
        """Run multiple commands in parallel."""
        return self._as_execution(self._post(f"/api/v1/sessions/{session_id}/exec", {
            "commands": commands,
            "parallel": True,
        }))

    def run_and_wait(self, session_id: str, command: str, args: list[str] | None = None, **kwargs) -> Execution:
        """Run a command and wait for completion."""
        ex = self.run(session_id, command, args, **kwargs)
        return self.wait_for_execution(session_id, ex.ID)

    def stream(self, session_id: str, command: str, args: list[str] | None = None, **kwargs) -> Generator[StreamEvent, None, None]:
        """Run a command and stream output as SSE events.

        Yields StreamEvent objects with .event and .data fields.
        Use .text for output text, .is_output / .is_done to filter.

        Example:
            for event in client.stream(sid, "python3", ["-c", "print('hi')"]):
                if event.is_output:
                    print(event.text, end="")
                if event.is_done:
                    break
        """
        body: dict[str, Any] = {"command": command}
        if args:
            body["args"] = args
        body.update(kwargs)
        yield from self._stream_sse(f"/api/v1/sessions/{session_id}/exec/stream", body)

    def stream_execution(self, session_id: str, exec_id: str) -> Generator[StreamEvent, None, None]:
        """Stream an already-running execution."""
        yield from self._stream_sse_get(f"/api/v1/sessions/{session_id}/exec/{exec_id}/stream")

    def _stream_sse(self, path: str, body: Any) -> Generator[StreamEvent, None, None]:
        headers = {"Content-Type": "application/json", "Accept": "text/event-stream"}
        if self._http.headers.get("Authorization"):
            headers["Authorization"] = self._http.headers["Authorization"]

        with httpx.stream("POST", f"{self._http.base_url}{path}",
                          json=body, headers=headers, timeout=300) as resp:
            yield from self._parse_sse(resp)

    def _stream_sse_get(self, path: str) -> Generator[StreamEvent, None, None]:
        headers = {"Accept": "text/event-stream"}
        if self._http.headers.get("Authorization"):
            headers["Authorization"] = self._http.headers["Authorization"]

        with httpx.stream("GET", f"{self._http.base_url}{path}",
                          headers=headers, timeout=300) as resp:
            yield from self._parse_sse(resp)

    def _parse_sse(self, resp) -> Generator[StreamEvent, None, None]:
        event_type = ""
        for line in resp.iter_lines():
            if line.startswith("event: "):
                event_type = line[7:]
            elif line.startswith("data: "):
                data_str = line[6:]
                try:
                    data = json.loads(data_str)
                except (json.JSONDecodeError, ValueError):
                    data = {"raw": data_str}
                evt = StreamEvent(event=event_type, data=data)
                yield evt
                if evt.is_done:
                    return
                event_type = ""

    def list_executions(self, session_id: str) -> list[Execution]:
        data = self._get(f"/api/v1/sessions/{session_id}/exec")
        return [self._as_execution(e) for e in data.get("executions", [])]

    def get_execution(self, session_id: str, exec_id: str) -> Execution:
        return self._as_execution(self._get(f"/api/v1/sessions/{session_id}/exec/{exec_id}"))

    def cancel_execution(self, session_id: str, exec_id: str) -> Execution:
        return self._as_execution(self._delete(f"/api/v1/sessions/{session_id}/exec/{exec_id}"))

    def approve_execution(self, session_id: str, exec_id: str, scope: str = "once") -> Execution:
        """Approve a pending command.

        Args:
            scope: "once" — approve this one execution only.
                   "command" — approve this command binary for the session.
                   "always" — permanently whitelist the command.
        """
        return self._as_execution(self._post(
            f"/api/v1/sessions/{session_id}/exec/{exec_id}/approve",
            {"scope": scope},
        ))

    def whitelist(self, session_id: str) -> dict:
        """Get the session's command whitelist and blocklist."""
        return self._get(f"/api/v1/sessions/{session_id}/whitelist")

    def update_whitelist(self, session_id: str, add: list[str] | None = None, remove: list[str] | None = None, block: list[str] | None = None) -> dict:
        """Update the session's command whitelist.

        Args:
            add: Commands to allow.
            remove: Commands to un-allow.
            block: Commands to permanently block.
        """
        return self._post(f"/api/v1/sessions/{session_id}/whitelist", {
            "add": add or [], "remove": remove or [], "block": block or [],
        })

    def reject_execution(self, session_id: str, exec_id: str, reason: str = "") -> Execution:
        return self._as_execution(self._post(
            f"/api/v1/sessions/{session_id}/exec/{exec_id}/reject",
            {"reason": reason},
        ))

    def wait_for_execution(self, session_id: str, exec_id: str, interval: float = 0.2, timeout: float = 60) -> Execution:
        """Poll until execution completes or needs approval."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            ex = self.get_execution(session_id, exec_id)
            if ex.Status in ("success", "failed", "canceled", "rejected", "pending_approval"):
                return ex
            time.sleep(interval)
        raise TimeoutError(f"Execution {exec_id} did not complete within {timeout}s")

    # ── Artifacts ────────────────────────────────────────

    def list_artifacts(self, session_id: str, checkpoint_id: str = "") -> list[Artifact]:
        """List files changed in a session.

        Args:
            session_id: Session ID.
            checkpoint_id: Optional checkpoint ID to filter by.
        """
        q = f"?checkpoint={checkpoint_id}" if checkpoint_id else ""
        data = self._get(f"/api/v1/sessions/{session_id}/artifacts{q}")
        return [self._as(Artifact, a) for a in data.get("artifacts", [])]

    def download_artifact(self, session_id: str, path: str) -> bytes:
        """Download a single file from a session.

        Args:
            session_id: Session ID.
            path: Path of the file inside the VM.

        Returns:
            Raw file contents as bytes.
        """
        resp = self._http.get(
            f"/api/v1/sessions/{session_id}/artifacts/download",
            params={"path": path},
        )
        if resp.is_error:
            try:
                data = resp.json()
                raise LokaError(data.get("error", f"HTTP {resp.status_code}"), resp.status_code)
            except (json.JSONDecodeError, ValueError):
                raise LokaError(f"HTTP {resp.status_code}", resp.status_code)
        return resp.content

    def download_artifacts(self, session_id: str, local_dir: str, checkpoint_id: str = "") -> None:
        """Download all artifacts as a tar archive and extract to a local directory.

        Args:
            session_id: Session ID.
            local_dir: Local directory to extract files into.
            checkpoint_id: Optional checkpoint ID.
        """
        import tarfile
        import io as _io
        import os as _os

        params: dict[str, str] = {"format": "tar"}
        if checkpoint_id:
            params["checkpoint"] = checkpoint_id
        resp = self._http.get(
            f"/api/v1/sessions/{session_id}/artifacts/download",
            params=params,
        )
        if resp.is_error:
            try:
                data = resp.json()
                raise LokaError(data.get("error", f"HTTP {resp.status_code}"), resp.status_code)
            except (json.JSONDecodeError, ValueError):
                raise LokaError(f"HTTP {resp.status_code}", resp.status_code)

        _os.makedirs(local_dir, exist_ok=True)
        with tarfile.open(fileobj=_io.BytesIO(resp.content), mode="r:*") as tf:
            tf.extractall(path=local_dir)

    # ── Checkpoints ──────────────────────────────────────

    def create_checkpoint(self, session_id: str, type: str = "light", label: str = "") -> Checkpoint:
        return self._as(Checkpoint, self._post(f"/api/v1/sessions/{session_id}/checkpoints", {
            "type": type, "label": label,
        }))

    def list_checkpoints(self, session_id: str) -> list[Checkpoint]:
        data = self._get(f"/api/v1/sessions/{session_id}/checkpoints")
        return [self._as(Checkpoint, c) for c in data.get("checkpoints", [])]

    def restore_checkpoint(self, session_id: str, checkpoint_id: str) -> Session:
        return self._as(Session, self._post(
            f"/api/v1/sessions/{session_id}/checkpoints/{checkpoint_id}/restore",
        ))

    def delete_checkpoint(self, session_id: str, checkpoint_id: str) -> None:
        self._delete(f"/api/v1/sessions/{session_id}/checkpoints/{checkpoint_id}")

    def diff_checkpoints(self, session_id: str, cp_a: str, cp_b: str) -> dict:
        return self._get(f"/api/v1/sessions/{session_id}/checkpoints/diff?a={cp_a}&b={cp_b}")

    # ── Images ───────────────────────────────────────────

    def pull_image(self, reference: str) -> Image:
        return self._as(Image, self._post("/api/v1/images/pull", {"reference": reference}))

    def list_images(self) -> list[Image]:
        data = self._get("/api/v1/images")
        return [self._as(Image, i) for i in data.get("images", [])]

    def get_image(self, image_id: str) -> Image:
        return self._as(Image, self._get(f"/api/v1/images/{image_id}"))

    def delete_image(self, image_id: str) -> None:
        self._delete(f"/api/v1/images/{image_id}")

    # ── Domains ─────────────────────────────────────────

    def list_domains(self) -> dict:
        return self._get("/api/v1/domains")

    # ── Workers ──────────────────────────────────────────

    def list_workers(self) -> list[Worker]:
        data = self._get("/api/v1/workers")
        return [self._as(Worker, w) for w in data.get("workers", [])]

    def drain_worker(self, worker_id: str, timeout_seconds: int = 300) -> Worker:
        return self._as(Worker, self._post(f"/api/v1/workers/{worker_id}/drain", {
            "timeout_seconds": timeout_seconds,
        }))

    # ── Health ───────────────────────────────────────────

    def health(self) -> dict:
        return self._get("/api/v1/health")

    # ── Services ─────────────────────────────────────────

    def deploy_service(self, wait: bool = True, timeout: float = 120, **kwargs) -> Service:
        """Deploy a new service.

        Args:
            wait: Wait for service to be ready (default True).
            timeout: Max seconds to wait.
            name: Service name.
            image: Docker image reference.
            port: Service port inside the VM.
            env: Environment variables dict.
            mounts: List of storage mounts.
            health_path: Health check URL path (default "/").
        """
        svc = self._as(Service, self._post("/api/v1/services", kwargs))
        if not wait:
            return svc
        deadline = time.time() + timeout
        while svc.Status not in ("running", "error", "terminated"):
            if time.time() > deadline:
                raise TimeoutError(f"service {svc.ID} not ready after {timeout}s (status: {svc.Status})")
            time.sleep(0.5)
            svc = self.get_service(svc.ID)
        if svc.Status == "error":
            raise RuntimeError(f"service failed: {svc.StatusMessage or 'unknown error'}")
        return svc

    def get_service(self, service_id: str) -> Service:
        return self._as(Service, self._get(f"/api/v1/services/{service_id}"))

    def list_services(self, status: str = "", name: str = "", limit: int = 0, offset: int = 0) -> list[Service]:
        params = []
        if status: params.append(f"status={status}")
        if name: params.append(f"name={name}")
        if limit: params.append(f"limit={limit}")
        if offset: params.append(f"offset={offset}")
        q = "?" + "&".join(params) if params else ""
        data = self._get(f"/api/v1/services{q}")
        return [self._as(Service, s) for s in data.get("services", [])]

    def destroy_service(self, service_id: str) -> None:
        self._delete(f"/api/v1/services/{service_id}")

    def stop_service(self, service_id: str) -> Service:
        return self._as(Service, self._post(f"/api/v1/services/{service_id}/stop"))

    def redeploy_service(self, service_id: str) -> Service:
        return self._as(Service, self._post(f"/api/v1/services/{service_id}/redeploy"))

    def update_service_env(self, service_id: str, env: dict[str, str]) -> Service:
        return self._as(Service, self._put(f"/api/v1/services/{service_id}/env", env))

    def get_service_logs(self, service_id: str, lines: int = 100) -> str:
        data = self._get(f"/api/v1/services/{service_id}/logs?lines={lines}")
        return data.get("logs", "")

    def add_service_route(self, service_id: str, domain: str, port: int = 0, protocol: str = "http") -> ServiceRoute:
        body: dict[str, Any] = {"domain": domain}
        if port: body["port"] = port
        if protocol != "http": body["protocol"] = protocol
        return self._as(ServiceRoute, self._post(f"/api/v1/services/{service_id}/routes", body))

    def remove_service_route(self, service_id: str, domain: str) -> None:
        self._delete(f"/api/v1/services/{service_id}/routes/{domain}")

    def list_service_routes(self, service_id: str) -> list[ServiceRoute]:
        data = self._get(f"/api/v1/services/{service_id}/routes")
        return [self._as(ServiceRoute, r) for r in data.get("routes", [])]

    # ── Volumes ──────────────────────────────────────────

    def create_volume(self, name: str, type: str = "network") -> VolumeRecord:
        return self._as(VolumeRecord, self._post("/api/v1/volumes", {"name": name, "type": type}))

    def list_volumes(self) -> list[VolumeRecord]:
        data = self._get("/api/v1/volumes")
        return [self._as(VolumeRecord, v) for v in data.get("volumes", [])]

    def get_volume(self, name: str) -> VolumeRecord:
        return self._as(VolumeRecord, self._get(f"/api/v1/volumes/{name}"))

    def delete_volume(self, name: str) -> None:
        self._delete(f"/api/v1/volumes/{name}")

    # ── Object Store ─────────────────────────────────────

    def objstore_put(self, bucket: str, key: str, data: bytes, content_type: str = "application/octet-stream") -> None:
        resp = self._http.put(
            f"/api/v1/objstore/objects/{bucket}/{key}",
            content=data,
            headers={"Content-Type": content_type},
        )
        if resp.is_error:
            raise LokaError(f"PUT object failed: HTTP {resp.status_code}", resp.status_code)

    def objstore_get(self, bucket: str, key: str) -> bytes:
        resp = self._http.get(f"/api/v1/objstore/objects/{bucket}/{key}")
        if resp.is_error:
            raise LokaError(f"GET object failed: HTTP {resp.status_code}", resp.status_code)
        return resp.content

    def objstore_head(self, bucket: str, key: str) -> bool:
        resp = self._http.head(f"/api/v1/objstore/objects/{bucket}/{key}")
        return resp.status_code == 200

    def objstore_delete(self, bucket: str, key: str) -> None:
        self._delete(f"/api/v1/objstore/objects/{bucket}/{key}")

    def objstore_list(self, bucket: str, prefix: str = "") -> list[ObjectInfo]:
        q = f"?prefix={prefix}" if prefix else ""
        data = self._get(f"/api/v1/objstore/list/{bucket}{q}")
        if isinstance(data, list):
            return [self._as(ObjectInfo, o) for o in data]
        return [self._as(ObjectInfo, o) for o in data.get("objects", data if isinstance(data, list) else [])]

    # ── Worker Tokens ────────────────────────────────────

    def create_worker_token(self, name: str, expires_seconds: int = 3600) -> WorkerToken:
        return self._as(WorkerToken, self._post("/api/v1/worker-tokens", {
            "name": name, "expires_seconds": expires_seconds,
        }))

    def list_worker_tokens(self) -> list[WorkerToken]:
        data = self._get("/api/v1/worker-tokens")
        return [self._as(WorkerToken, t) for t in data.get("tokens", [])]

    def revoke_worker_token(self, token_id: str) -> None:
        self._delete(f"/api/v1/worker-tokens/{token_id}")

    # ── Admin ────────────────────────────────────────────

    def trigger_gc(self, dry_run: bool = False) -> dict:
        return self._post("/api/v1/admin/gc", {"dry_run": dry_run})

    def gc_status(self) -> dict:
        return self._get("/api/v1/admin/gc/status")

    def retention_config(self) -> dict:
        return self._get("/api/v1/admin/retention")

    def toggle_dns(self, enabled: bool) -> dict:
        return self._post("/api/v1/admin/dns", {"enabled": enabled})

    def raft_status(self) -> dict:
        return self._get("/api/debug/raft")

    # ── Workers (extended) ───────────────────────────────

    def get_worker(self, worker_id: str) -> Worker:
        return self._as(Worker, self._get(f"/api/v1/workers/{worker_id}"))

    def undrain_worker(self, worker_id: str) -> Worker:
        return self._as(Worker, self._post(f"/api/v1/workers/{worker_id}/undrain"))

    def label_worker(self, worker_id: str, labels: dict[str, str]) -> Worker:
        return self._as(Worker, self._put(f"/api/v1/workers/{worker_id}/labels", {"labels": labels}))

    def remove_worker(self, worker_id: str, force: bool = False) -> None:
        q = "?force=true" if force else ""
        self._delete(f"/api/v1/workers/{worker_id}{q}")

    # ── Providers ────────────────────────────────────────

    def list_providers(self) -> list[dict]:
        data = self._get("/api/v1/providers")
        return data.get("providers", [])

    def provision_workers(self, provider: str, count: int = 1, **config) -> list[Worker]:
        data = self._post(f"/api/v1/providers/{provider}/provision", {"count": count, **config})
        return [self._as(Worker, w) for w in data.get("workers", [])]

    def deprovision_worker(self, provider: str, worker_id: str) -> None:
        self._delete(f"/api/v1/providers/{provider}/workers/{worker_id}")

    def provider_status(self, provider: str) -> dict:
        return self._get(f"/api/v1/providers/{provider}/status")

    # ── Sessions (extended) ──────────────────────────────

    def migrate_session(self, session_id: str, target_worker_id: str) -> Session:
        return self._as(Session, self._post(f"/api/v1/sessions/{session_id}/migrate", {
            "target_worker_id": target_worker_id,
        }))

    # ── HTTP ─────────────────────────────────────────────

    def _get(self, path: str) -> dict:
        resp = self._http.get(path)
        return self._handle(resp)

    def _post(self, path: str, body: Any = None) -> dict:
        resp = self._http.post(path, json=body or {})
        return self._handle(resp)

    def _put(self, path: str, body: Any = None) -> dict:
        resp = self._http.put(path, json=body or {})
        return self._handle(resp)

    def _delete(self, path: str) -> dict:
        resp = self._http.delete(path)
        if resp.status_code == 204:
            return {}
        return self._handle(resp)

    def _handle(self, resp: httpx.Response) -> dict:
        if resp.status_code == 204:
            return {}
        data = resp.json()
        if resp.is_error:
            raise LokaError(data.get("error", f"HTTP {resp.status_code}"), resp.status_code)
        return data

    def _as(self, cls, data: dict):
        if not data:
            return cls()
        obj = cls()
        for k, v in data.items():
            if hasattr(obj, k):
                setattr(obj, k, v)
        return obj

    def _as_execution(self, data: dict) -> Execution:
        ex = self._as(Execution, data)
        if isinstance(data.get("Results"), list):
            ex.Results = [self._as(CommandResult, r) for r in data["Results"]]
        return ex


class LokaError(Exception):
    def __init__(self, message: str, status_code: int = 0):
        super().__init__(message)
        self.status_code = status_code
