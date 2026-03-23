package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
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

	// Phase 3: All approved — start all processes.
	procs := make([]*RunningProcess, 0, len(commands))
	for _, cmd := range commands {
		proc, err := e.startProcess(ctx, cmd)
		if err != nil {
			for _, p := range procs {
				p.Cancel()
			}
			return nil, VerdictBlocked, fmt.Errorf("start command %s: %w", cmd.ID, err)
		}
		procs = append(procs, proc)
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

	osCmd := exec.CommandContext(procCtx, cmd.Command, cmd.Args...)
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

		proc.Result = &loka.CommandResult{
			CommandID: cmd.ID,
			ExitCode:  -1,
			Stderr:    fmt.Sprintf("failed to start: %v", err),
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
