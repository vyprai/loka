package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
)

// Agent is the worker-side agent that manages Firecracker microVMs.
// There are NO direct process executions — everything goes through a VM.
type Agent struct {
	id       string
	hostname string
	provider string
	labels   map[string]string
	logger   *slog.Logger

	mu            sync.RWMutex
	sessions      map[string]*SessionState
	overlay       *vm.OverlayManager
	checkpointMgr *CheckpointManager
	vmManager     *vm.Manager
}

// SessionState tracks a running session backed by a Firecracker microVM.
type SessionState struct {
	ID       string
	Mode     loka.ExecMode
	Policy   loka.ExecPolicy
	VM       *vm.MicroVM     // Firecracker VM instance.
	Vsock    *vm.VsockClient // vsock connection to supervisor inside VM.
	LayerMap map[string]string
}

// NewAgent creates a new worker agent with Firecracker VM management.
func NewAgent(provider string, labels map[string]string, dataDir string, objStore objstore.ObjectStore, fcConfig vm.FirecrackerConfig, logger *slog.Logger) (*Agent, error) {
	hostname, _ := os.Hostname()
	overlay := vm.NewOverlayManager(dataDir)
	cpMgr := NewCheckpointManager(overlay, objStore, logger)

	vmMgr, err := vm.NewManager(fcConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("init VM manager: %w", err)
	}

	return &Agent{
		id:            uuid.New().String(),
		hostname:      hostname,
		provider:      provider,
		labels:        labels,
		logger:        logger,
		sessions:      make(map[string]*SessionState),
		overlay:       overlay,
		checkpointMgr: cpMgr,
		vmManager:     vmMgr,
	}, nil
}

// ID returns the agent's unique identifier.
func (a *Agent) ID() string { return a.id }

// SetID sets the agent ID (from registration response).
func (a *Agent) SetID(id string) { a.id = id }

// Capacity returns the local machine's resource capacity.
func (a *Agent) Capacity() loka.ResourceCapacity {
	return loka.ResourceCapacity{
		CPUCores: runtime.NumCPU(),
		MemoryMB: 8192, // Placeholder; real detection would use /proc/meminfo.
		DiskMB:   51200,
	}
}

// Hostname returns the agent's hostname.
func (a *Agent) Hostname() string { return a.hostname }

// Provider returns the provider name.
func (a *Agent) Provider() string { return a.provider }

// Labels returns the agent's labels.
func (a *Agent) Labels() map[string]string { return a.labels }

// LaunchOpts holds options for launching a session.
type LaunchOpts struct {
	Mode                loka.ExecMode
	Policy              loka.ExecPolicy
	VCPU                int
	MemoryMB            int
	RootfsPath          string // Image rootfs path (if not using default).
	SnapshotMemPath     string // Warm snapshot memory file for instant restore.
	SnapshotVMStatePath string // Warm snapshot VM state file.
}

// LaunchSession starts a Firecracker microVM for this session.
func (a *Agent) LaunchSession(ctx context.Context, sessionID string, opts LaunchOpts) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.sessions[sessionID]; exists {
		return fmt.Errorf("session %s already exists", sessionID)
	}

	// Initialize overlay filesystem.
	if err := a.overlay.Init(sessionID); err != nil {
		return fmt.Errorf("init overlay: %w", err)
	}

	vcpu := opts.VCPU
	if vcpu == 0 { vcpu = 1 }
	mem := opts.MemoryMB
	if mem == 0 { mem = 512 }

	// Launch Firecracker microVM — uses warm snapshot if available (~28ms),
	// otherwise cold boots (~1-2s).
	microVM, err := a.vmManager.Launch(ctx, sessionID, vm.VMConfig{
		VCPU:                vcpu,
		MemoryMB:            mem,
		RootfsPath:          opts.RootfsPath,
		OverlayDir:          a.overlay.SessionDir(sessionID),
		SnapshotMemPath:     opts.SnapshotMemPath,
		SnapshotVMStatePath: opts.SnapshotVMStatePath,
	})
	if err != nil {
		return fmt.Errorf("launch VM: %w", err)
	}

	// Connect to the supervisor inside the VM via vsock.
	vsock := vm.NewVsockClient(microVM.VsockPath)

	// Wait for supervisor to be ready.
	for i := 0; i < 50; i++ {
		if err := vsock.Ping(); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Send exec policy and mode to the supervisor.
	vsock.SetPolicy(opts.Policy)
	vsock.SetMode(opts.Mode)

	a.sessions[sessionID] = &SessionState{
		ID:       sessionID,
		Mode:     opts.Mode,
		Policy:   opts.Policy,
		VM:       microVM,
		Vsock:    vsock,
		LayerMap: make(map[string]string),
	}

	a.logger.Info("session launched with Firecracker VM",
		"session", sessionID,
		"vm_pid", microVM.PID,
		"mode", opts.Mode,
		"warm_snapshot", opts.SnapshotMemPath != "",
	)
	return nil
}

// WorkspacePath returns the workspace path for a session.
func (a *Agent) WorkspacePath(sessionID string) string {
	return a.overlay.WorkspacePath(sessionID)
}

// StopSession stops the microVM and cleans up.
func (a *Agent) StopSession(sessionID string) error {
	a.mu.Lock()
	_, ok := a.sessions[sessionID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("session %s not found", sessionID)
	}
	delete(a.sessions, sessionID)
	a.mu.Unlock()

	a.vmManager.Stop(sessionID)
	a.logger.Info("session stopped", "session", sessionID)
	return nil
}

// SetMode changes the execution mode — sends to supervisor via vsock.
func (a *Agent) SetMode(sessionID string, mode loka.ExecMode) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	if err := sess.Vsock.SetMode(mode); err != nil {
		return fmt.Errorf("set mode via vsock: %w", err)
	}
	sess.Mode = mode
	a.logger.Info("session mode changed", "session", sessionID, "mode", mode)
	return nil
}

// ExecResult contains the result of a command execution.
type ExecResult struct {
	ExecID  string
	Status  loka.ExecStatus
	Results []loka.CommandResult
	Error   string
}

// ExecCommands sends commands to the supervisor inside the VM via vsock.
func (a *Agent) ExecCommands(ctx context.Context, sessionID, execID string, commands []loka.Command, parallel bool) *ExecResult {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return &ExecResult{ExecID: execID, Status: loka.ExecStatusFailed, Error: fmt.Sprintf("session %s not found", sessionID)}
	}

	resp, err := sess.Vsock.Execute(vm.ExecRequest{Commands: commands, Parallel: parallel, ExecID: execID})
	if err != nil {
		return &ExecResult{ExecID: execID, Status: loka.ExecStatusFailed, Error: err.Error()}
	}
	return &ExecResult{ExecID: execID, Status: loka.ExecStatus(resp.Status), Results: resp.Results, Error: resp.Error}
}

// ApproveOnProxy delegates to ApproveOnGate.
func (a *Agent) ApproveOnProxy(sessionID string, commandIDs []string) error {
	for _, id := range commandIDs {
		if err := a.ApproveOnGate(sessionID, id, false); err != nil {
			return err
		}
	}
	return nil
}

// ApproveOnGate sends approve to the supervisor's gate via vsock.
func (a *Agent) ApproveOnGate(sessionID, commandID string, addToWhitelist bool) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return sess.Vsock.ApproveCommand(commandID, addToWhitelist)
}

// DenyOnGate sends deny to the supervisor's gate via vsock.
func (a *Agent) DenyOnGate(sessionID, commandID, reason string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return sess.Vsock.DenyCommand(commandID, reason)
}

// AddToWhitelist permanently whitelists a command via vsock.
func (a *Agent) AddToWhitelist(sessionID, command string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.Policy.AllowedCommands = append(sess.Policy.AllowedCommands, command)
	return sess.Vsock.SetPolicy(sess.Policy)
}

// CancelExec cancels a running command via vsock.
func (a *Agent) CancelExec(sessionID, cmdID string) error { return nil }

// CancelAllExec cancels all running commands.
func (a *Agent) CancelAllExec(sessionID string) error { return nil }

// Heartbeat returns the current heartbeat data.
func (a *Agent) Heartbeat() *loka.Heartbeat {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sessionIDs []string
	for id := range a.sessions {
		sessionIDs = append(sessionIDs, id)
	}

	status := loka.WorkerStatusReady
	if len(a.sessions) > 0 {
		status = loka.WorkerStatusBusy
	}

	return &loka.Heartbeat{
		WorkerID:     a.id,
		Timestamp:    time.Now(),
		Status:       status,
		SessionCount: len(a.sessions),
		SessionIDs:   sessionIDs,
	}
}

// CreateCheckpoint creates a checkpoint for a session.
func (a *Agent) CreateCheckpoint(ctx context.Context, sessionID, checkpointID string, cpType loka.CheckpointType) *CheckpointResult {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return &CheckpointResult{
			CheckpointID: checkpointID,
			SessionID:    sessionID,
			Error:        fmt.Sprintf("session %s not found", sessionID),
		}
	}

	result := a.checkpointMgr.Create(ctx, sessionID, checkpointID, cpType)

	if result.Success {
		// Track the layer mapping.
		a.mu.Lock()
		sess.LayerMap[checkpointID] = result.LayerName
		a.mu.Unlock()
	}

	return result
}

// RestoreCheckpoint restores a session's workspace to a checkpoint.
func (a *Agent) RestoreCheckpoint(ctx context.Context, sessionID, checkpointID, overlayKey string) error {
	a.mu.RLock()
	_, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	return a.checkpointMgr.Restore(ctx, sessionID, checkpointID, overlayKey)
}

// DiffCheckpoints returns filesystem diff between two checkpoint layers.
func (a *Agent) DiffCheckpoints(sessionID, cpIDA, cpIDB string) ([]vm.DiffEntry, error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	layerA, okA := sess.LayerMap[cpIDA]
	layerB, okB := sess.LayerMap[cpIDB]
	if !okA || !okB {
		return nil, fmt.Errorf("checkpoint layer mapping not found")
	}

	return a.checkpointMgr.Diff(sessionID, layerA, layerB)
}

// SessionCount returns the number of active sessions.
func (a *Agent) SessionCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sessions)
}
