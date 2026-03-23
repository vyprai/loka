import type {
  Session, CreateSessionOpts, Execution, RunOpts, ExecMode,
  Checkpoint, CheckpointType, Image, Worker, StreamEvent, SyncResult, Artifact,
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

    if (!wait || session.Ready) return session;

    const deadline = Date.now() + timeout * 1000;
    while (!session.Ready && session.Status !== 'error') {
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
      if (session.Ready) return session;
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

  /** Expose a session port on a public subdomain. */
  async exposeSession(sessionId: string, subdomain: string, remotePort: number): Promise<{ subdomain: string; url: string; port: number }> {
    return this.post(`/api/v1/sessions/${sessionId}/expose`, { subdomain, remote_port: remotePort });
  }

  /** Remove a previously exposed subdomain from a session. */
  async unexposeSession(sessionId: string, subdomain: string): Promise<void> {
    await this.del(`/api/v1/sessions/${sessionId}/expose/${subdomain}`);
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
