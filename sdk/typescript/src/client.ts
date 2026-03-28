import type {
  Session, CreateSessionOpts, Execution, RunOpts, ExecMode,
  Checkpoint, CheckpointType, Image, Worker, StreamEvent, SyncResult, Artifact,
  Service, DeployServiceOpts, ServiceRoute, VolumeRecord, WorkerToken, ObjectInfo,
} from './types';

export interface LokaClientOpts {
  baseUrl?: string;
  token?: string;
  timeout?: number;
}

export class LokaClient {
  private baseUrl: string;
  private token: string;
  private timeout: number;

  constructor(opts: LokaClientOpts = {}) {
    this.baseUrl = (opts.baseUrl || 'http://localhost:6840').replace(/\/$/, '');
    this.token = opts.token || '';
    this.timeout = opts.timeout || 30000;
  }

  // ── Sessions ────────────────────────────────────────

  async createSession(opts: CreateSessionOpts = {}): Promise<Session> {
    const { wait = true, timeout = 120, ...createOpts } = opts;
    let session: Session = await this.post('/api/v1/sessions', createOpts);

    if (!wait || session.Ready || session.Status === 'running') return session;

    const deadline = Date.now() + timeout * 1000;
    while (!session.Ready && session.Status !== 'running' && session.Status !== 'error') {
      if (Date.now() > deadline) {
        throw new Error(`Session ${session.ID} not ready after ${timeout}s (status: ${session.Status})`);
      }
      await new Promise(r => setTimeout(r, 500));
      session = await this.getSession(session.ID);
    }
    if (session.Status === 'error') {
      throw new Error(`Session failed: ${session.StatusMessage || 'unknown error'}`);
    }
    return session;
  }

  async getSession(id: string): Promise<Session> {
    return this.get(`/api/v1/sessions/${id}`);
  }

  async listSessions(status?: string): Promise<{ sessions: Session[]; total: number }> {
    const q = status ? `?status=${status}` : '';
    return this.get(`/api/v1/sessions${q}`);
  }

  async destroySession(id: string): Promise<void> {
    await this.del(`/api/v1/sessions/${id}`);
  }

  /** Sync data between a session's storage mount and the object store. */
  async syncMount(sessionId: string, opts: {
    mount_path: string;
    direction: 'push' | 'pull';
    prefix?: string;
    delete?: boolean;
    dry_run?: boolean;
  }): Promise<SyncResult> {
    return this.post(`/api/v1/sessions/${sessionId}/sync`, opts);
  }

  /**
   * Mount a local directory into a session via gRPC tunnel.
   * Requires gRPC streaming — use the CLI for this:
   *   loka session mount <id> <local-path> <vm-path>
   */
  async mountLocal(_sessionId: string, _localPath: string, _vmPath: string, _opts?: { readOnly?: boolean }): Promise<void> {
    throw new Error(
      'mountLocal requires gRPC streaming. Use the CLI: loka session mount <id> <local-path> <vm-path>'
    );
  }

  /**
   * Forward a local port to a port inside the session VM.
   * Opens a local TCP listener and tunnels connections via gRPC streaming.
   * Requires gRPC streaming — use the CLI for this:
   *   loka session port-forward <id> <local>:<remote>
   */
  async portForward(_sessionId: string, _localPort: number, _remotePort: number): Promise<void> {
    throw new Error(
      'portForward requires gRPC streaming. Use the CLI: loka session port-forward <id> <local>:<remote>'
    );
  }

  async pauseSession(id: string): Promise<Session> {
    return this.post(`/api/v1/sessions/${id}/pause`, {});
  }

  async resumeSession(id: string): Promise<Session> {
    return this.post(`/api/v1/sessions/${id}/resume`, {});
  }

  async idleSession(id: string): Promise<Session> {
    return this.post(`/api/v1/sessions/${id}/idle`, {});
  }

  /** Poll getSession until the session is ready, with a configurable timeout. */
  async waitUntilReady(sessionId: string, opts?: { timeout?: number }): Promise<Session> {
    const timeout = opts?.timeout ?? 120;
    const deadline = Date.now() + timeout * 1000;
    while (true) {
      const session = await this.getSession(sessionId);
      if (session.Ready || session.Status === 'running') return session;
      if (session.Status === 'error') {
        throw new Error(`Session failed: ${session.StatusMessage || 'unknown error'}`);
      }
      if (Date.now() > deadline) {
        throw new Error(`Session ${sessionId} not ready after ${timeout}s (status: ${session.Status})`);
      }
      await new Promise(r => setTimeout(r, 500));
    }
  }

  async setMode(sessionId: string, mode: ExecMode): Promise<Session> {
    return this.post(`/api/v1/sessions/${sessionId}/mode`, { mode });
  }

  // ── Command Execution ───────────────────────────────

  async run(sessionId: string, opts: RunOpts): Promise<Execution> {
    return this.post(`/api/v1/sessions/${sessionId}/exec`, opts);
  }

  /** Run multiple commands in parallel within a session. */
  async runParallel(sessionId: string, commands: Array<{ command: string; args?: string[]; workdir?: string }>): Promise<Execution> {
    return this.post(`/api/v1/sessions/${sessionId}/exec`, { commands, parallel: true });
  }

  /** Shorthand: run a single command and wait for result. */
  async runCommand(sessionId: string, command: string, args: string[] = [], opts: { workdir?: string; env?: Record<string, string> } = {}): Promise<Execution> {
    const ex = await this.run(sessionId, { command, args, ...opts });
    return this.waitForExecution(sessionId, ex.ID);
  }

  /**
   * Stream command output via SSE. Starts execution and yields events.
   *
   * @example
   * for await (const event of loka.stream(sessionId, { command: 'python3', args: ['-c', 'print("hi")'] })) {
   *   if (event.event === 'output') process.stdout.write(event.data.text);
   *   if (event.event === 'done') break;
   * }
   */
  async *stream(sessionId: string, opts: RunOpts): AsyncGenerator<StreamEvent> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json', 'Accept': 'text/event-stream' };
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;

    const resp = await fetch(`${this.baseUrl}/api/v1/sessions/${sessionId}/exec/stream`, {
      method: 'POST',
      headers,
      body: JSON.stringify(opts),
    });

    if (!resp.ok || !resp.body) throw new Error(`Stream failed: HTTP ${resp.status}`);

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let currentEvent = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';

      for (const line of lines) {
        if (line.startsWith('event: ')) {
          currentEvent = line.slice(7);
        } else if (line.startsWith('data: ')) {
          try {
            const data = JSON.parse(line.slice(6));
            const evt: StreamEvent = { event: currentEvent as StreamEvent['event'], data };
            yield evt;
            if (evt.event === 'done') return;
          } catch {}
          currentEvent = '';
        }
      }
    }
  }

  /** Stream an already-running execution. */
  async *streamExecution(sessionId: string, execId: string): AsyncGenerator<StreamEvent> {
    const headers: Record<string, string> = { 'Accept': 'text/event-stream' };
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;

    const resp = await fetch(`${this.baseUrl}/api/v1/sessions/${sessionId}/exec/${execId}/stream`, { headers });
    if (!resp.ok || !resp.body) throw new Error(`Stream failed: HTTP ${resp.status}`);

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let currentEvent = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';

      for (const line of lines) {
        if (line.startsWith('event: ')) currentEvent = line.slice(7);
        else if (line.startsWith('data: ')) {
          try {
            yield { event: currentEvent as StreamEvent['event'], data: JSON.parse(line.slice(6)) };
          } catch {}
          currentEvent = '';
        }
      }
    }
  }

  async getExecution(sessionId: string, execId: string): Promise<Execution> {
    return this.get(`/api/v1/sessions/${sessionId}/exec/${execId}`);
  }

  async listExecutions(sessionId: string): Promise<{ executions: Execution[]; total: number }> {
    return this.get(`/api/v1/sessions/${sessionId}/exec`);
  }

  async cancelExecution(sessionId: string, execId: string): Promise<Execution> {
    return this.del(`/api/v1/sessions/${sessionId}/exec/${execId}`);
  }

  /**
   * Approve a pending command.
   * @param scope "once" (this execution only), "command" (this binary for the session), "always" (permanent whitelist)
   */
  async approveExecution(sessionId: string, execId: string, scope: 'once' | 'command' | 'always' = 'once'): Promise<Execution> {
    return this.post(`/api/v1/sessions/${sessionId}/exec/${execId}/approve`, { scope });
  }

  /** Get the session's command whitelist and blocklist. */
  async getWhitelist(sessionId: string): Promise<{ allowed_commands: string[]; blocked_commands: string[] }> {
    return this.get(`/api/v1/sessions/${sessionId}/whitelist`);
  }

  /** Update the session's command whitelist. */
  async updateWhitelist(sessionId: string, opts: { add?: string[]; remove?: string[]; block?: string[] }): Promise<{ allowed_commands: string[]; blocked_commands: string[] }> {
    return this.post(`/api/v1/sessions/${sessionId}/whitelist`, opts) as any;
  }

  async rejectExecution(sessionId: string, execId: string, reason = ''): Promise<Execution> {
    return this.post(`/api/v1/sessions/${sessionId}/exec/${execId}/reject`, { reason });
  }

  /** Poll until execution reaches a terminal state. */
  async waitForExecution(sessionId: string, execId: string, intervalMs = 200, timeoutMs = 60000): Promise<Execution> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const ex = await this.getExecution(sessionId, execId);
      if (['success', 'failed', 'canceled', 'rejected'].includes(ex.Status)) return ex;
      if (ex.Status === 'pending_approval') return ex; // Caller must approve/reject.
      await new Promise(r => setTimeout(r, intervalMs));
    }
    throw new Error(`Execution ${execId} did not complete within ${timeoutMs}ms`);
  }

  // ── Checkpoints ─────────────────────────────────────

  async createCheckpoint(sessionId: string, type: CheckpointType = 'light', label = ''): Promise<Checkpoint> {
    return this.post(`/api/v1/sessions/${sessionId}/checkpoints`, { type, label });
  }

  async listCheckpoints(sessionId: string): Promise<{ checkpoints: Checkpoint[]; root: string; current: string }> {
    return this.get(`/api/v1/sessions/${sessionId}/checkpoints`);
  }

  async restoreCheckpoint(sessionId: string, checkpointId: string): Promise<Session> {
    return this.post(`/api/v1/sessions/${sessionId}/checkpoints/${checkpointId}/restore`, {});
  }

  async deleteCheckpoint(sessionId: string, checkpointId: string): Promise<void> {
    await this.del(`/api/v1/sessions/${sessionId}/checkpoints/${checkpointId}`);
  }

  /** Diff two checkpoints, returning the changes between them. */
  async diffCheckpoints(sessionId: string, cpA: string, cpB: string): Promise<any> {
    return this.get(`/api/v1/sessions/${sessionId}/checkpoints/diff?a=${cpA}&b=${cpB}`);
  }

  // ── Artifacts ─────────────────────────────────────────

  /** List files changed in a session. */
  async listArtifacts(sessionId: string, checkpointId?: string): Promise<{ artifacts: Artifact[]; total: number }> {
    const q = checkpointId ? `?checkpoint=${checkpointId}` : '';
    return this.get(`/api/v1/sessions/${sessionId}/artifacts${q}`);
  }

  /** Download a single file from a session as raw binary. */
  async downloadArtifact(sessionId: string, path: string): Promise<ArrayBuffer> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeout);

    const headers: Record<string, string> = {};
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;

    try {
      const resp = await fetch(`${this.baseUrl}/api/v1/sessions/${sessionId}/artifacts/download?path=${encodeURIComponent(path)}`, {
        method: 'GET',
        headers,
        signal: controller.signal,
      });

      if (!resp.ok) {
        let msg = `HTTP ${resp.status}`;
        try {
          const data = await resp.json() as Record<string, string>;
          if (data.error) msg = data.error;
        } catch {}
        throw new Error(msg);
      }
      return resp.arrayBuffer();
    } finally {
      clearTimeout(timer);
    }
  }

  // ── Domains / Expose ──────────────────────────────────

  async listDomains(): Promise<{ routes: any[]; base_domain: string }> {
    return this.get('/api/v1/domains');
  }

  /** Expose a session port on a public domain. */
  async exposeSession(sessionId: string, domain: string, remotePort: number): Promise<{ domain: string; url: string; port: number }> {
    return this.post(`/api/v1/sessions/${sessionId}/expose`, { domain, remote_port: remotePort });
  }

  /** Remove a previously exposed domain from a session. */
  async unexposeSession(sessionId: string, domain: string): Promise<void> {
    await this.del(`/api/v1/sessions/${sessionId}/expose/${domain}`);
  }

  // ── Images ──────────────────────────────────────────

  async pullImage(reference: string): Promise<Image> {
    return this.post('/api/v1/images/pull', { reference });
  }

  async listImages(): Promise<{ images: Image[]; total: number }> {
    return this.get('/api/v1/images');
  }

  async getImage(id: string): Promise<Image> {
    return this.get(`/api/v1/images/${id}`);
  }

  async deleteImage(id: string): Promise<void> {
    await this.del(`/api/v1/images/${id}`);
  }

  // ── Workers ─────────────────────────────────────────

  async listWorkers(): Promise<{ workers: Worker[]; total: number }> {
    return this.get('/api/v1/workers');
  }

  async drainWorker(id: string, timeoutSeconds = 300): Promise<Worker> {
    return this.post(`/api/v1/workers/${id}/drain`, { timeout_seconds: timeoutSeconds });
  }

  // ── Services ─────────────────────────────────────────

  async deployService(opts: DeployServiceOpts): Promise<Service> {
    const { wait = true, timeout = 120, ...deployOpts } = opts;
    let svc: Service = await this.post('/api/v1/services', deployOpts);
    if (!wait) return svc;
    const deadline = Date.now() + timeout * 1000;
    while (svc.Status !== 'running' && svc.Status !== 'error' && svc.Status !== 'terminated') {
      if (Date.now() > deadline) throw new Error(`Service ${svc.ID} not ready after ${timeout}s (status: ${svc.Status})`);
      await new Promise(r => setTimeout(r, 500));
      svc = await this.getService(svc.ID);
    }
    if (svc.Status === 'error') throw new Error(`Service failed: ${svc.StatusMessage || 'unknown error'}`);
    return svc;
  }

  async getService(id: string): Promise<Service> {
    return this.get(`/api/v1/services/${id}`);
  }

  async listServices(opts?: { status?: string; name?: string; limit?: number; offset?: number }): Promise<{ services: Service[]; total: number }> {
    const params = new URLSearchParams();
    if (opts?.status) params.set('status', opts.status);
    if (opts?.name) params.set('name', opts.name);
    if (opts?.limit) params.set('limit', String(opts.limit));
    if (opts?.offset) params.set('offset', String(opts.offset));
    const q = params.toString() ? `?${params}` : '';
    return this.get(`/api/v1/services${q}`);
  }

  async destroyService(id: string): Promise<void> {
    await this.del(`/api/v1/services/${id}`);
  }

  async stopService(id: string): Promise<Service> {
    return this.post(`/api/v1/services/${id}/stop`, {});
  }

  async redeployService(id: string): Promise<Service> {
    return this.post(`/api/v1/services/${id}/redeploy`, {});
  }

  async updateServiceEnv(id: string, env: Record<string, string>): Promise<Service> {
    return this.put(`/api/v1/services/${id}/env`, env);
  }

  async getServiceLogs(id: string, lines = 100): Promise<string> {
    const data = await this.get<{ logs: string }>(`/api/v1/services/${id}/logs?lines=${lines}`);
    return data.logs || '';
  }

  async addServiceRoute(id: string, domain: string, opts?: { port?: number; protocol?: string }): Promise<ServiceRoute> {
    return this.post(`/api/v1/services/${id}/routes`, { domain, ...opts });
  }

  async removeServiceRoute(id: string, domain: string): Promise<void> {
    await this.del(`/api/v1/services/${id}/routes/${domain}`);
  }

  async listServiceRoutes(id: string): Promise<{ routes: ServiceRoute[] }> {
    return this.get(`/api/v1/services/${id}/routes`);
  }

  // ── Volumes ──────────────────────────────────────────

  async createVolume(name: string, type = 'network'): Promise<VolumeRecord> {
    return this.post('/api/v1/volumes', { name, type });
  }

  async listVolumes(): Promise<{ volumes: VolumeRecord[] }> {
    return this.get('/api/v1/volumes');
  }

  async getVolume(name: string): Promise<VolumeRecord> {
    return this.get(`/api/v1/volumes/${name}`);
  }

  async deleteVolume(name: string): Promise<void> {
    await this.del(`/api/v1/volumes/${name}`);
  }

  // ── Object Store ─────────────────────────────────────

  async objStorePut(bucket: string, key: string, data: ArrayBuffer | string, contentType = 'application/octet-stream'): Promise<void> {
    const headers: Record<string, string> = { 'Content-Type': contentType };
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;
    const resp = await fetch(`${this.baseUrl}/api/v1/objstore/objects/${bucket}/${key}`, {
      method: 'PUT', headers, body: data,
    });
    if (!resp.ok) throw new Error(`PUT object failed: HTTP ${resp.status}`);
  }

  async objStoreGet(bucket: string, key: string): Promise<ArrayBuffer> {
    const headers: Record<string, string> = {};
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;
    const resp = await fetch(`${this.baseUrl}/api/v1/objstore/objects/${bucket}/${key}`, { headers });
    if (!resp.ok) throw new Error(`GET object failed: HTTP ${resp.status}`);
    return resp.arrayBuffer();
  }

  async objStoreHead(bucket: string, key: string): Promise<boolean> {
    const headers: Record<string, string> = {};
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;
    const resp = await fetch(`${this.baseUrl}/api/v1/objstore/objects/${bucket}/${key}`, { method: 'HEAD', headers });
    return resp.status === 200;
  }

  async objStoreDelete(bucket: string, key: string): Promise<void> {
    await this.del(`/api/v1/objstore/objects/${bucket}/${key}`);
  }

  async objStoreList(bucket: string, prefix?: string): Promise<ObjectInfo[]> {
    const q = prefix ? `?prefix=${encodeURIComponent(prefix)}` : '';
    const data = await this.get<any>(`/api/v1/objstore/list/${bucket}${q}`);
    return Array.isArray(data) ? data : (data.objects || []);
  }

  // ── Worker Tokens ────────────────────────────────────

  async createWorkerToken(name: string, expiresSeconds = 3600): Promise<WorkerToken> {
    return this.post('/api/v1/worker-tokens', { name, expires_seconds: expiresSeconds });
  }

  async listWorkerTokens(): Promise<{ tokens: WorkerToken[] }> {
    return this.get('/api/v1/worker-tokens');
  }

  async revokeWorkerToken(tokenId: string): Promise<void> {
    await this.del(`/api/v1/worker-tokens/${tokenId}`);
  }

  // ── Admin ────────────────────────────────────────────

  async triggerGC(dryRun = false): Promise<any> {
    return this.post('/api/v1/admin/gc', { dry_run: dryRun });
  }

  async gcStatus(): Promise<any> {
    return this.get('/api/v1/admin/gc/status');
  }

  async retentionConfig(): Promise<any> {
    return this.get('/api/v1/admin/retention');
  }

  async toggleDNS(enabled: boolean): Promise<any> {
    return this.post('/api/v1/admin/dns', { enabled });
  }

  async raftStatus(): Promise<any> {
    return this.get('/api/debug/raft');
  }

  // ── Workers (extended) ───────────────────────────────

  async getWorker(id: string): Promise<Worker> {
    return this.get(`/api/v1/workers/${id}`);
  }

  async undrainWorker(id: string): Promise<Worker> {
    return this.post(`/api/v1/workers/${id}/undrain`, {});
  }

  async labelWorker(id: string, labels: Record<string, string>): Promise<Worker> {
    return this.put(`/api/v1/workers/${id}/labels`, { labels });
  }

  async removeWorker(id: string, force = false): Promise<void> {
    const q = force ? '?force=true' : '';
    await this.del(`/api/v1/workers/${id}${q}`);
  }

  // ── Providers ────────────────────────────────────────

  async listProviders(): Promise<{ providers: any[] }> {
    return this.get('/api/v1/providers');
  }

  async provisionWorkers(provider: string, count = 1, config?: Record<string, any>): Promise<{ workers: Worker[] }> {
    return this.post(`/api/v1/providers/${provider}/provision`, { count, ...config });
  }

  async deprovisionWorker(provider: string, workerId: string): Promise<void> {
    await this.del(`/api/v1/providers/${provider}/workers/${workerId}`);
  }

  async providerStatus(provider: string): Promise<any> {
    return this.get(`/api/v1/providers/${provider}/status`);
  }

  async migrateSession(id: string, targetWorkerId: string): Promise<Session> {
    return this.post(`/api/v1/sessions/${id}/migrate`, { target_worker_id: targetWorkerId });
  }

  // ── Health ──────────────────────────────────────────

  async health(): Promise<{ status: string; workers_total: number; workers_ready: number }> {
    return this.get('/api/v1/health');
  }

  // ── HTTP ────────────────────────────────────────────

  private async get<T>(path: string): Promise<T> {
    return this.request('GET', path);
  }

  private async post<T>(path: string, body: unknown): Promise<T> {
    return this.request('POST', path, body);
  }

  private async del<T>(path: string): Promise<T> {
    return this.request('DELETE', path);
  }

  private async put<T>(path: string, body: unknown): Promise<T> {
    return this.request('PUT', path, body);
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeout);

    const headers: Record<string, string> = {};
    if (body) headers['Content-Type'] = 'application/json';
    if (this.token) headers['Authorization'] = `Bearer ${this.token}`;

    try {
      const resp = await fetch(`${this.baseUrl}${path}`, {
        method,
        headers,
        body: body ? JSON.stringify(body) : undefined,
        signal: controller.signal,
      });

      if (resp.status === 204) return undefined as T;

      const data = await resp.json() as Record<string, any>;
      if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
      return data as T;
    } finally {
      clearTimeout(timer);
    }
  }
}
