package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

// Executor spawns and manages real OS processes.
// ALL commands are routed through the CommandProxy before execution.
// The Sandbox enforces OS-level restrictions (PATH, mounts, seccomp).
//
// In production, this runs inside the guest VM as the only process spawner.
// For MVP, it runs locally on the host as a stand-in.
type Executor struct {
	mu        sync.Mutex
	processes map[string]*RunningProcess
	proxy     *CommandProxy
	sandbox   *Sandbox
	gate      *ApprovalGate
	sessionID string
}

// RunningProcess tracks a running OS process.
type RunningProcess struct {
	ID        string
	Cmd       *exec.Cmd
	Stdout    *bytes.Buffer
	Stderr    *bytes.Buffer
	Cancel    context.CancelFunc
	StartedAt time.Time
	Done      chan struct{}
	Result    *loka.CommandResult
}

// NewExecutor creates a new process executor with command proxy, sandbox, and approval gate.
func NewExecutor(policy loka.ExecPolicy, mode loka.ExecMode, envDir, dataDir, sessionID string, onPending func(*PendingApproval)) *Executor {
	sandbox := NewSandbox(policy, mode, envDir, dataDir)
	sandbox.Apply()
	return &Executor{
		processes: make(map[string]*RunningProcess),
		proxy:     NewCommandProxy(policy, mode),
		sandbox:   sandbox,
		gate:      NewApprovalGate(5*time.Minute, onPending),
		sessionID: sessionID,
	}
}

// Gate returns the approval gate for external access (approve/deny).
func (e *Executor) Gate() *ApprovalGate {
	return e.gate
}

// SetMode changes the execution mode on both the proxy and sandbox.
func (e *Executor) SetMode(mode loka.ExecMode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.proxy.SetMode(mode)
	e.sandbox.SetMode(mode) // Re-applies OS-level restrictions.
}

// GetMode returns the current execution mode.
func (e *Executor) GetMode() loka.ExecMode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.proxy.mode
}

// SetPolicy updates the exec policy on the proxy.
func (e *Executor) SetPolicy(policy loka.ExecPolicy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.proxy.SetPolicy(policy)
}

// GetAuditLog returns the proxy's audit trail.
func (e *Executor) GetAuditLog() []AuditEntry {
	return e.proxy.GetAuditLog()
}

// ApproveOnce marks a command ID as one-shot approved on the proxy.
func (e *Executor) ApproveOnce(cmdID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.proxy.ApproveOnce(cmdID)
}

// AddToWhitelist permanently adds a command to the proxy whitelist.
func (e *Executor) AddToWhitelist(command string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.proxy.AddToWhitelist(command)
}

// ErrNeedsApproval is returned when a command requires agent approval before execution.
var ErrNeedsApproval = fmt.Errorf("needs_approval")

// ExecuteResult wraps the command result with the proxy verdict.
type ExecuteResult struct {
	Result  *loka.CommandResult
	Verdict Verdict
	Reason  string // Explanation for needs_approval or blocked.
}

// Execute runs a single command through the proxy.
//
// Flow:
//   - Allowed: executes immediately, blocks until done.
//   - Blocked: returns error immediately.
//   - NeedsApproval: SUSPENDS at the approval gate, blocks the goroutine
//     until the agent approves (resumes execution) or denies (returns error).
func (e *Executor) Execute(ctx context.Context, cmd loka.Command) (*ExecuteResult, error) {
	v := e.proxy.Validate(cmd)

	switch v.Verdict {
	case VerdictBlocked:
		return &ExecuteResult{
			Result: &loka.CommandResult{
				CommandID: cmd.ID,
				ExitCode:  -1,
				Stderr:    fmt.Sprintf("BLOCKED by command proxy: %s", v.Reason),
				StartedAt: time.Now(),
				EndedAt:   time.Now(),
			},
			Verdict: VerdictBlocked,
			Reason:  v.Reason,
		}, fmt.Errorf("command proxy: %s", v.Reason)

	case VerdictNeedsApproval:
		// SUSPEND: block here until the agent approves or denies.
		decision, err := e.gate.Suspend(ctx, e.sessionID, cmd, v.Reason)
		if err != nil {
			return &ExecuteResult{
				Result: &loka.CommandResult{
					CommandID: cmd.ID,
					ExitCode:  -1,
					Stderr:    fmt.Sprintf("DENIED/TIMEOUT: %v", err),
					StartedAt: time.Now(),
					EndedAt:   time.Now(),
				},
				Verdict: VerdictBlocked,
				Reason:  err.Error(),
			}, err
		}

		// Approved — optionally add to whitelist.
		if decision.AddToWhitelist {
			e.proxy.AddToWhitelist(filepath.Base(cmd.Command))
		}
		// Fall through to execute.

	case VerdictAllowed:
		// Proceed directly.
	}

	proc, err := e.startProcess(ctx, cmd)
	if err != nil {
		return nil, err
	}

	<-proc.Done
	return &ExecuteResult{
		Result:  proc.Result,
		Verdict: VerdictAllowed,
	}, nil
}

// ExecuteParallel runs multiple commands concurrently.
// If ANY command needs approval, ALL commands are suspended at the gate.
// The batch resumes only when all pending commands are approved.
func (e *Executor) ExecuteParallel(ctx context.Context, commands []loka.Command) ([]loka.CommandResult, Verdict, error) {
	// Phase 1: Validate all commands, collect those needing approval.
	type cmdVerdict struct {
		cmd loka.Command
		v   *ValidationResult
	}
	verdicts := make([]cmdVerdict, len(commands))
	var needsApproval []loka.Command

	for i, cmd := range commands {
		v := e.proxy.Validate(cmd)
		verdicts[i] = cmdVerdict{cmd: cmd, v: v}

		switch v.Verdict {
		case VerdictBlocked:
			return nil, VerdictBlocked, fmt.Errorf("command proxy blocked %q: %s", cmd.Command, v.Reason)
		case VerdictNeedsApproval:
			needsApproval = append(needsApproval, cmd)
		}
	}

	// Phase 2: If any need approval, suspend ALL of them at the gate concurrently.
	if len(needsApproval) > 0 {
		type gateResult struct {
			cmd      loka.Command
			decision *ApprovalDecision
			err      error
		}
		results := make(chan gateResult, len(needsApproval))

		for _, cmd := range needsApproval {
			cmd := cmd
			go func() {
				reason := fmt.Sprintf("command %q is not whitelisted — requires approval", cmd.Command)
				decision, err := e.gate.Suspend(ctx, e.sessionID, cmd, reason)
				results <- gateResult{cmd: cmd, decision: decision, err: err}
			}()
		}

		// Wait for all approvals/denials.
		for range needsApproval {
			gr := <-results
			if gr.err != nil {
				// One command was denied/timed out — cancel all.
				return nil, VerdictBlocked, fmt.Errorf("command %q: %w", gr.cmd.Command, gr.err)
			}
			if gr.decision.AddToWhitelist {
				e.proxy.AddToWhitelist(filepath.Base(gr.cmd.Command))
			}
		}
	}

	// Phase 3: All approved — start all processes, collecting any errors.
	// Start all processes first, then cancel all if any failed to start.
	// This avoids a race where a process could start after the cancel loop.
	procs := make([]*RunningProcess, 0, len(commands))
	var startErrors []error
	for _, cmd := range commands {
		proc, err := e.startProcess(ctx, cmd)
		if err != nil {
			startErrors = append(startErrors, fmt.Errorf("start command %s: %w", cmd.ID, err))
			continue
		}
		procs = append(procs, proc)
	}
	if len(startErrors) > 0 {
		for _, p := range procs {
			p.Cancel()
		}
		return nil, VerdictBlocked, startErrors[0]
	}

	cmdResults := make([]loka.CommandResult, len(procs))
	for i, proc := range procs {
		<-proc.Done
		cmdResults[i] = *proc.Result
	}

	return cmdResults, VerdictAllowed, nil
}

// Cancel cancels a running command.
func (e *Executor) Cancel(cmdID string) error {
	e.mu.Lock()
	proc, ok := e.processes[cmdID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("process %s not found", cmdID)
	}
	proc.Cancel()
	return nil
}

// CancelAll cancels all running commands.
func (e *Executor) CancelAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, proc := range e.processes {
		proc.Cancel()
	}
}

// ListProcesses returns all running processes.
func (e *Executor) ListProcesses() []*RunningProcess {
	e.mu.Lock()
	defer e.mu.Unlock()
	procs := make([]*RunningProcess, 0, len(e.processes))
	for _, p := range e.processes {
		procs = append(procs, p)
	}
	return procs
}

func (e *Executor) startProcess(ctx context.Context, cmd loka.Command) (*RunningProcess, error) {
	procCtx, cancel := context.WithCancel(ctx)

	// Resolve binary path using the restricted PATH (not the supervisor's own PATH).
	cmdPath := cmd.Command
	if !filepath.IsAbs(cmdPath) {
		env := e.proxy.ProcessEnv(cmd)
		for _, e := range env {
			if len(e) > 5 && e[:5] == "PATH=" {
				for _, dir := range filepath.SplitList(e[5:]) {
					candidate := filepath.Join(dir, cmdPath)
					if _, err := os.Stat(candidate); err == nil {
						cmdPath = candidate
						break
					}
				}
				break
			}
		}
	}

	osCmd := exec.CommandContext(procCtx, cmdPath, cmd.Args...)
	if cmd.Workdir != "" {
		osCmd.Dir = cmd.Workdir
	}

	// Environment controlled by proxy — restricted PATH, blocked env vars, hardened defaults.
	osCmd.Env = e.proxy.ProcessEnv(cmd)

	var stdout, stderr bytes.Buffer
	osCmd.Stdout = &stdout
	osCmd.Stderr = &stderr

	proc := &RunningProcess{
		ID:        cmd.ID,
		Cmd:       osCmd,
		Stdout:    &stdout,
		Stderr:    &stderr,
		Cancel:    cancel,
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}

	e.mu.Lock()
	e.processes[cmd.ID] = proc
	e.mu.Unlock()

	if err := osCmd.Start(); err != nil {
		cancel()
		close(proc.Done)
		e.mu.Lock()
		delete(e.processes, cmd.ID)
		e.mu.Unlock()

		// Include context about the binary, workdir, and PATH for easier debugging.
		var restrictedPATH string
		for _, envVar := range osCmd.Env {
			if len(envVar) > 5 && envVar[:5] == "PATH=" {
				restrictedPATH = envVar[5:]
				break
			}
		}
		proc.Result = &loka.CommandResult{
			CommandID: cmd.ID,
			ExitCode:  -1,
			Stderr:    fmt.Sprintf("exec %q in %q (PATH=%s): %v", cmdPath, osCmd.Dir, restrictedPATH, err),
			StartedAt: proc.StartedAt,
			EndedAt:   time.Now(),
		}
		return proc, nil
	}

	go func() {
		err := osCmd.Wait()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}

		proc.Result = &loka.CommandResult{
			CommandID: cmd.ID,
			ExitCode:  exitCode,
			Stdout:    stdout.String(),
			Stderr:    stderr.String(),
			StartedAt: proc.StartedAt,
			EndedAt:   time.Now(),
		}

		e.mu.Lock()
		delete(e.processes, cmd.ID)
		e.mu.Unlock()

		close(proc.Done)
	}()

	return proc, nil
}

// ── Ring Buffer ──────────────────────────────────────────

// RingBuffer is a fixed-capacity circular buffer that stores the last N lines.
type RingBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
	pos   int
	full  bool
	// partial holds an incomplete line (no trailing newline yet).
	partial string
}

// NewRingBuffer creates a ring buffer that holds up to cap lines.
func NewRingBuffer(cap int) *RingBuffer {
	return &RingBuffer{
		lines: make([]string, cap),
		cap:   cap,
	}
}

// WriteLine adds a single line to the buffer.
func (rb *RingBuffer) WriteLine(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.lines[rb.pos] = line
	rb.pos = (rb.pos + 1) % rb.cap
	if rb.pos == 0 {
		rb.full = true
	}
}

// Write implements io.Writer. It splits input on newlines and stores each line.
func (rb *RingBuffer) Write(p []byte) (n int, err error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	data := rb.partial + string(p)
	rb.partial = ""

	for {
		idx := strings.IndexByte(data, '\n')
		if idx < 0 {
			rb.partial = data
			break
		}
		line := data[:idx]
		data = data[idx+1:]
		rb.lines[rb.pos] = line
		rb.pos = (rb.pos + 1) % rb.cap
		if rb.pos == 0 {
			rb.full = true
		}
	}
	return len(p), nil
}

// Lines returns the last n lines (or all lines if n <= 0 or n > stored count).
func (rb *RingBuffer) Lines(n int) []string {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	total := rb.pos
	if rb.full {
		total = rb.cap
	}
	if n <= 0 || n > total {
		n = total
	}
	if n == 0 {
		return nil
	}

	result := make([]string, n)
	start := rb.pos - n
	if start < 0 {
		start += rb.cap
	}
	for i := 0; i < n; i++ {
		result[i] = rb.lines[(start+i)%rb.cap]
	}
	return result
}

// Flush writes the partial buffer (incomplete line) as a complete line.
// Call this before shutdown to avoid losing trailing data without a newline.
func (rb *RingBuffer) Flush() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if len(rb.partial) > 0 {
		rb.lines[rb.pos] = rb.partial
		rb.pos = (rb.pos + 1) % rb.cap
		if rb.pos == 0 {
			rb.full = true
		}
		rb.partial = ""
	}
}

// ── Service Process ─────────────────────────────────────

// ServiceProcess manages a long-running service process with restart policies.
type ServiceProcess struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	pid         int
	running     bool
	exitCode    int
	startedAt   time.Time
	restarts    int
	lastRestart time.Time

	// Ring buffer for logs (last 10K lines).
	stdout *RingBuffer
	stderr *RingBuffer

	// Config.
	command       string
	args          []string
	env           map[string]string
	workdir       string
	restartPolicy string // "on-failure", "always", "never"

	cancel  context.CancelFunc
	ctx     context.Context
	stopped chan struct{} // closed when the process loop has fully exited
}

// ServiceProcessStatus describes the current state of the service.
type ServiceProcessStatus struct {
	Running       bool      `json:"running"`
	PID           int       `json:"pid"`
	ExitCode      int       `json:"exit_code"`
	Restarts      int       `json:"restarts"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	StartedAt     time.Time `json:"started_at"`
}

// NewServiceProcess creates a new service process (not yet started).
func NewServiceProcess(command string, args []string, env map[string]string, workdir, restartPolicy string) *ServiceProcess {
	return &ServiceProcess{
		command:       command,
		args:          args,
		env:           env,
		workdir:       workdir,
		restartPolicy: restartPolicy,
		stdout:        NewRingBuffer(10000),
		stderr:        NewRingBuffer(10000),
		stopped:       make(chan struct{}),
	}
}

// Start starts the service process and its restart loop.
func (sp *ServiceProcess) Start(parentCtx context.Context) error {
	sp.mu.Lock()
	if sp.running {
		sp.mu.Unlock()
		return fmt.Errorf("service already running")
	}
	sp.mu.Unlock()

	ctx, cancel := context.WithCancel(parentCtx)
	sp.mu.Lock()
	sp.ctx = ctx
	sp.cancel = cancel
	sp.mu.Unlock()

	if err := sp.startOnce(); err != nil {
		cancel()
		return err
	}

	// Restart loop goroutine.
	go sp.restartLoop()
	return nil
}

func (sp *ServiceProcess) startOnce() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	cmdPath := sp.command
	osCmd := exec.CommandContext(sp.ctx, cmdPath, sp.args...)
	if sp.workdir != "" {
		osCmd.Dir = sp.workdir
	}

	// Build environment. For service processes running inside the sandboxed VM,
	// we allow more env vars (NODE_OPTIONS, PYTHONPATH etc.) since the VM itself
	// is the security boundary.
	envSlice := []string{
		"PATH=/env/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=/workspace",
	}
	for k, v := range sp.env {
		// Only block truly dangerous vars that could escape the sandbox.
		upper := strings.ToUpper(k)
		if upper == "LD_PRELOAD" || upper == "LD_LIBRARY_PATH" {
			continue
		}
		envSlice = append(envSlice, fmt.Sprintf("%s=%s", k, v))
	}
	osCmd.Env = envSlice
	osCmd.Stdout = sp.stdout
	osCmd.Stderr = sp.stderr

	if err := osCmd.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}

	sp.cmd = osCmd
	sp.pid = osCmd.Process.Pid
	sp.running = true
	sp.startedAt = time.Now()
	return nil
}

func (sp *ServiceProcess) restartLoop() {
	defer close(sp.stopped)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	const maxRestarts = 5
	const maxRestartWindow = 5 * time.Minute

	// Track recent failure timestamps to detect rapid restart loops.
	var recentFailures []time.Time

	for {
		// Wait for the current process to exit.
		sp.mu.Lock()
		cmd := sp.cmd
		sp.mu.Unlock()

		var exitCode int
		if cmd != nil && cmd.Process != nil {
			err := cmd.Wait()
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
		}

		sp.mu.Lock()
		sp.running = false
		sp.exitCode = exitCode
		sp.mu.Unlock()

		// Check if we should restart.
		select {
		case <-sp.ctx.Done():
			return
		default:
		}

		shouldRestart := false
		sp.mu.Lock()
		policy := sp.restartPolicy
		sp.mu.Unlock()

		switch policy {
		case "always":
			shouldRestart = true
		case "on-failure":
			shouldRestart = exitCode != 0
		default: // "never"
			return
		}

		if !shouldRestart {
			return
		}

		// Track this failure and check for restart loop.
		now := time.Now()
		recentFailures = append(recentFailures, now)
		// Prune failures outside the window.
		cutoff := now.Add(-maxRestartWindow)
		pruned := recentFailures[:0]
		for _, t := range recentFailures {
			if t.After(cutoff) {
				pruned = append(pruned, t)
			}
		}
		recentFailures = pruned

		if len(recentFailures) >= maxRestarts {
			// Too many restarts within the window — stop restarting.
			fmt.Fprintf(os.Stderr, "service exceeded max restarts (%d in %s) — stopping\n", maxRestarts, maxRestartWindow)
			return
		}

		// Backoff before restart.
		select {
		case <-sp.ctx.Done():
			return
		case <-time.After(backoff):
		}

		sp.mu.Lock()
		sp.restarts++
		sp.lastRestart = time.Now()
		sp.mu.Unlock()

		if err := sp.startOnce(); err != nil {
			// Failed to restart — log the error and update status.
			sp.mu.Lock()
			sp.running = false
			sp.mu.Unlock()
			fmt.Fprintf(os.Stderr, "service restart failed permanently: %v\n", err)
			return
		}

		// Exponential backoff: 1s, 2s, 4s, 8s, ... max 30s.
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Stop sends a signal to the service process, then SIGKILL after timeout.
func (sp *ServiceProcess) Stop(signal syscall.Signal, timeout time.Duration) error {
	sp.mu.Lock()
	cancel := sp.cancel
	cmd := sp.cmd
	running := sp.running
	stopped := sp.stopped
	stdout := sp.stdout
	stderr := sp.stderr
	sp.mu.Unlock()

	if !running || cmd == nil || cmd.Process == nil {
		if cancel != nil {
			cancel()
		}
		// Flush partial log lines before returning.
		stdout.Flush()
		stderr.Flush()
		return nil
	}

	// Send the requested signal (typically SIGTERM).
	if err := cmd.Process.Signal(signal); err != nil {
		// Process may have already exited.
		if cancel != nil {
			cancel()
		}
		stdout.Flush()
		stderr.Flush()
		return nil
	}

	// Wait for graceful shutdown or timeout.
	select {
	case <-stopped:
		stdout.Flush()
		stderr.Flush()
		return nil
	case <-time.After(timeout):
		// Force kill.
		cmd.Process.Signal(syscall.SIGKILL)
		if cancel != nil {
			cancel()
		}
		<-stopped
		stdout.Flush()
		stderr.Flush()
		return nil
	}
}

// Restart stops and restarts the service.
func (sp *ServiceProcess) Restart() error {
	if err := sp.Stop(syscall.SIGTERM, 10*time.Second); err != nil {
		return err
	}
	// Create new stopped channel and context for fresh restart loop.
	sp.mu.Lock()
	sp.stopped = make(chan struct{})
	sp.mu.Unlock()

	ctx := context.Background()
	newCtx, cancel := context.WithCancel(ctx)
	sp.mu.Lock()
	sp.ctx = newCtx
	sp.cancel = cancel
	sp.mu.Unlock()

	if err := sp.startOnce(); err != nil {
		cancel()
		return err
	}
	go sp.restartLoop()
	return nil
}

// Status returns the current state of the service.
func (sp *ServiceProcess) Status() ServiceProcessStatus {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	var uptime float64
	if sp.running && !sp.startedAt.IsZero() {
		uptime = time.Since(sp.startedAt).Seconds()
	}

	return ServiceProcessStatus{
		Running:       sp.running,
		PID:           sp.pid,
		ExitCode:      sp.exitCode,
		Restarts:      sp.restarts,
		UptimeSeconds: uptime,
		StartedAt:     sp.startedAt,
	}
}
