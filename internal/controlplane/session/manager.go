package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/metrics"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// CreateOpts holds options for creating a new session.
type CreateOpts struct {
	Name         string
	ImageRef     string // Docker image: "ubuntu:22.04"
	SnapshotID   string // Optional: restore from snapshot.
	Mode         loka.ExecMode
	VCPUs        int
	MemoryMB     int
	Labels       map[string]string
	WorkerLabels map[string]string // Scheduling affinity labels.
	ExecPolicy   *loka.ExecPolicy  // Command restrictions. Nil = default policy.
}

// Manager orchestrates session lifecycle.
type Manager struct {
	store     store.Store
	registry  *worker.Registry
	scheduler *scheduler.Scheduler
	images    *image.Manager
	logger    *slog.Logger
}

// NewManager creates a new session manager.
func NewManager(s store.Store, reg *worker.Registry, sched *scheduler.Scheduler, imgMgr *image.Manager, logger *slog.Logger) *Manager {
	return &Manager{store: s, registry: reg, scheduler: sched, images: imgMgr, logger: logger}
}

// Create creates a new session and schedules it to a worker.
func (m *Manager) Create(ctx context.Context, opts CreateOpts) (*loka.Session, error) {
	if opts.Mode == "" {
		opts.Mode = loka.ModeExplore
	}
	if opts.VCPUs == 0 {
		opts.VCPUs = 1
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = 512
	}
	if opts.Labels == nil {
		opts.Labels = make(map[string]string)
	}

	// Set exec policy.
	execPolicy := loka.DefaultExecPolicy()
	if opts.ExecPolicy != nil {
		execPolicy = *opts.ExecPolicy
	}

	now := time.Now()
	s := &loka.Session{
		ID:         uuid.New().String(),
		Name:       opts.Name,
		Status:     loka.SessionStatusCreating,
		Mode:       opts.Mode,
		ImageRef:   opts.ImageRef,
		SnapshotID: opts.SnapshotID,
		VCPUs:      opts.VCPUs,
		MemoryMB:   opts.MemoryMB,
		Labels:     opts.Labels,
		ExecPolicy: execPolicy,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := m.store.Sessions().Create(ctx, s); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	metrics.SessionsCreated.Inc()
	metrics.SessionsTotal.WithLabelValues("creating").Inc()
	m.logger.Info("session created", "id", s.ID, "name", s.Name)

	// Schedule to a worker via scheduler.
	constraints := scheduler.Constraints{
		RequireLabels: opts.WorkerLabels,
	}
	wConn, err := m.scheduler.Pick(constraints)
	if err != nil {
		// No workers available — still mark as running for dev mode.
		m.logger.Warn("no workers available, session will run without worker", "id", s.ID)
		s.Status = loka.SessionStatusRunning
	} else {
		s.WorkerID = wConn.Worker.ID

		// Resolve image — get rootfs and warm snapshot paths.
		launchData := worker.LaunchSessionData{
			SessionID:  s.ID,
			ImageRef:   s.ImageRef,
			Mode:       s.Mode,
			ExecPolicy: s.ExecPolicy,
			VCPUs:      s.VCPUs,
			MemoryMB:   s.MemoryMB,
		}
		if m.images != nil {
			if img, ok := m.images.GetByRef(s.ImageRef); ok {
				s.ImageID = img.ID
				launchData.RootfsPath = img.RootfsPath
				launchData.SnapshotMemPath = img.SnapshotMem
				launchData.SnapshotVMStatePath = img.SnapshotVMState
			}
		}

		m.registry.SendCommand(wConn.Worker.ID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "launch_session",
			Data: launchData,
		})
		s.Status = loka.SessionStatusRunning
		m.logger.Info("session scheduled to worker", "session", s.ID, "worker", wConn.Worker.ID)
	}

	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, fmt.Errorf("update session status: %w", err)
	}

	return s, nil
}

// Get retrieves a session by ID.
func (m *Manager) Get(ctx context.Context, id string) (*loka.Session, error) {
	return m.store.Sessions().Get(ctx, id)
}

// List returns sessions matching the filter.
func (m *Manager) List(ctx context.Context, filter store.SessionFilter) ([]*loka.Session, error) {
	return m.store.Sessions().List(ctx, filter)
}

// Destroy terminates and deletes a session.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return err
	}

	// Send stop command to worker if assigned.
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "stop_session",
			Data: map[string]string{"session_id": s.ID},
		})
	}

	s.Status = loka.SessionStatusTerminated
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	metrics.SessionsDestroyed.Inc()
	m.logger.Info("session destroyed", "id", id)
	return nil
}

// Pause pauses a running session.
func (m *Manager) Pause(ctx context.Context, id string) (*loka.Session, error) {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.CanTransitionTo(loka.SessionStatusPaused) {
		return nil, fmt.Errorf("cannot pause session in status %s", s.Status)
	}
	s.Status = loka.SessionStatusPaused
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Resume resumes a paused session.
func (m *Manager) Resume(ctx context.Context, id string) (*loka.Session, error) {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.CanTransitionTo(loka.SessionStatusRunning) {
		return nil, fmt.Errorf("cannot resume session in status %s", s.Status)
	}
	s.Status = loka.SessionStatusRunning
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// SetMode changes the execution mode of a session.
func (m *Manager) SetMode(ctx context.Context, id string, mode loka.ExecMode) (*loka.Session, error) {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !loka.CanTransitionModeTo(s.Mode, mode) {
		return nil, fmt.Errorf("cannot transition from mode %s to %s", s.Mode, mode)
	}

	// Send mode change to worker.
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "set_mode",
			Data: map[string]interface{}{"session_id": s.ID, "mode": string(mode)},
		})
	}

	s.Mode = mode
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, err
	}
	m.logger.Info("session mode changed", "id", id, "mode", mode)
	return s, nil
}

// Exec creates and dispatches a command execution to the appropriate worker.
// Commands are validated against the session's ExecPolicy and current mode.
func (m *Manager) Exec(ctx context.Context, sessionID string, commands []loka.Command, parallel bool) (*loka.Execution, error) {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if s.Status != loka.SessionStatusRunning {
		return nil, fmt.Errorf("session is not running (status: %s)", s.Status)
	}

	// Pre-flight check: basic validation at API layer.
	// Deep enforcement (shell parsing, interpreter scanning) happens
	// in the supervisor command proxy inside the microVM.
	for _, cmd := range commands {
		if err := s.ExecPolicy.ValidateCommand(cmd, s.Mode); err != nil {
			return nil, fmt.Errorf("policy violation: %w", err)
		}
	}

	if s.ExecPolicy.MaxParallel > 0 && parallel && len(commands) > s.ExecPolicy.MaxParallel {
		return nil, fmt.Errorf("too many parallel commands: %d (max %d)", len(commands), s.ExecPolicy.MaxParallel)
	}

	now := time.Now()
	exec := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		Status:    loka.ExecStatusPending,
		Parallel:  parallel,
		Commands:  commands,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := m.store.Executions().Create(ctx, exec); err != nil {
		return nil, fmt.Errorf("create execution: %w", err)
	}

	// Determine initial status.
	// In ask mode or when approval is required, the command will be dispatched
	// to the worker but the executor's gate will SUSPEND it until approved.
	needsApproval := s.ExecPolicy.RequiresApproval(s.Mode) || s.Mode == loka.ModeAsk
	if needsApproval {
		exec.Status = loka.ExecStatusPendingApproval
	} else {
		exec.Status = loka.ExecStatusRunning
	}
	m.store.Executions().Update(ctx, exec)

	// Always dispatch to worker — the gate handles suspension.
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "exec",
			Data: worker.ExecCommandData{
				SessionID: sessionID,
				ExecID:    exec.ID,
				Commands:  commands,
				Parallel:  parallel,
			},
		})
	} else {
		exec.Status = loka.ExecStatusFailed
		exec.UpdatedAt = time.Now()
		m.store.Executions().Update(ctx, exec)
		return exec, fmt.Errorf("no worker assigned to session")
	}

	metrics.ExecsTotal.Inc()
	m.logger.Info("execution dispatched", "id", exec.ID, "session", sessionID, "worker", s.WorkerID)
	return exec, nil
}

// ApproveExecution approves a pending_approval execution.
// This sends an "approve_gate" command to the worker, which resumes the
// SUSPENDED goroutine in the executor. The command then proceeds to execute.
// If addToWhitelist is true, the command is permanently whitelisted.
func (m *Manager) ApproveExecution(ctx context.Context, sessionID, execID string, addToWhitelist ...bool) (*loka.Execution, error) {
	exec, err := m.store.Executions().Get(ctx, execID)
	if err != nil {
		return nil, err
	}
	if exec.Status != loka.ExecStatusPendingApproval {
		return nil, fmt.Errorf("execution is not pending approval (status: %s)", exec.Status)
	}

	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if s.WorkerID == "" {
		return nil, fmt.Errorf("no worker assigned to session")
	}

	wantWhitelist := len(addToWhitelist) > 0 && addToWhitelist[0]

	// Send approve to the gate for each command — this RESUMES the suspended goroutine.
	for _, cmd := range exec.Commands {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "approve_gate",
			Data: worker.ApproveOnGateData{
				SessionID:      sessionID,
				CommandID:      cmd.ID,
				AddToWhitelist: wantWhitelist,
			},
		})
	}

	// Update the session's whitelist if requested.
	if wantWhitelist {
		for _, cmd := range exec.Commands {
			binary := extractBinary(cmd.Command)
			s.ExecPolicy.AllowedCommands = append(s.ExecPolicy.AllowedCommands, binary)
		}
		s.UpdatedAt = time.Now()
		m.store.Sessions().Update(ctx, s)
	}

	// The execution status will be updated by the worker when the command finishes.
	// For now, mark it as running (the gate unblocks the goroutine).
	exec.Status = loka.ExecStatusRunning
	m.store.Executions().Update(ctx, exec)

	metrics.ExecsTotal.Inc()
	m.logger.Info("execution approved — resuming suspended command", "id", execID, "session", sessionID)
	return exec, nil
}

func extractBinary(command string) string {
	parts := strings.Split(command, "/")
	return parts[len(parts)-1]
}

// RejectExecution rejects a pending_approval execution.
// This sends a "deny_gate" command to the worker, which unblocks the
// suspended goroutine with an error. The command returns a denial result.
func (m *Manager) RejectExecution(ctx context.Context, sessionID, execID, reason string) (*loka.Execution, error) {
	exec, err := m.store.Executions().Get(ctx, execID)
	if err != nil {
		return nil, err
	}
	if exec.Status != loka.ExecStatusPendingApproval {
		return nil, fmt.Errorf("execution is not pending approval (status: %s)", exec.Status)
	}

	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if reason == "" {
		reason = "denied by operator"
	}

	// Send deny to the gate for each command — this UNBLOCKS the suspended goroutine with an error.
	if s.WorkerID != "" {
		for _, cmd := range exec.Commands {
			m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
				ID:   uuid.New().String(),
				Type: "deny_gate",
				Data: worker.DenyOnGateData{
					SessionID: sessionID,
					CommandID: cmd.ID,
					Reason:    reason,
				},
			})
		}
	}

	exec.Status = loka.ExecStatusRejected
	exec.Results = []loka.CommandResult{{
		CommandID: "system",
		ExitCode:  -1,
		Stderr:    "execution rejected: " + reason,
		StartedAt: time.Now(),
		EndedAt:   time.Now(),
	}}
	exec.UpdatedAt = time.Now()
	m.store.Executions().Update(ctx, exec)

	m.logger.Info("execution rejected — aborting suspended command", "id", execID, "reason", reason)
	return exec, nil
}

// CompleteExecution updates an execution with results from the worker.
func (m *Manager) CompleteExecution(ctx context.Context, execID string, status loka.ExecStatus, results []loka.CommandResult, errMsg string) error {
	exec, err := m.store.Executions().Get(ctx, execID)
	if err != nil {
		return err
	}
	exec.Status = status
	exec.Results = results
	exec.UpdatedAt = time.Now()
	if err := m.store.Executions().Update(ctx, exec); err != nil {
		return err
	}
	metrics.ExecsByStatus.WithLabelValues(string(status)).Inc()
	if len(results) > 0 {
		duration := results[len(results)-1].EndedAt.Sub(results[0].StartedAt).Seconds()
		if duration > 0 {
			metrics.ExecDuration.Observe(duration)
		}
	}
	m.logger.Info("execution completed", "id", execID, "status", status)
	return nil
}

// CancelExecution cancels a running execution.
func (m *Manager) CancelExecution(ctx context.Context, sessionID, execID string) (*loka.Execution, error) {
	exec, err := m.store.Executions().Get(ctx, execID)
	if err != nil {
		return nil, err
	}
	if exec.IsTerminal() {
		return exec, nil // Already done.
	}

	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Send cancel to worker.
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "cancel_exec",
			Data: map[string]string{"session_id": sessionID, "exec_id": execID},
		})
	}

	exec.Status = loka.ExecStatusCanceled
	exec.UpdatedAt = time.Now()
	if err := m.store.Executions().Update(ctx, exec); err != nil {
		return nil, err
	}
	return exec, nil
}

// CreateCheckpoint dispatches a checkpoint creation to the worker.
func (m *Manager) CreateCheckpoint(ctx context.Context, sessionID, checkpointID string, cpType loka.CheckpointType, parentID string) (*loka.Checkpoint, error) {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	cp := &loka.Checkpoint{
		ID:        checkpointID,
		SessionID: sessionID,
		ParentID:  parentID,
		Type:      cpType,
		Status:    loka.CheckpointStatusCreating,
		CreatedAt: time.Now(),
	}

	if err := m.store.Checkpoints().Create(ctx, cp); err != nil {
		return nil, fmt.Errorf("create checkpoint: %w", err)
	}

	// Dispatch to worker.
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "create_checkpoint",
			Data: worker.CreateCheckpointData{
				SessionID:    sessionID,
				CheckpointID: checkpointID,
				Type:         cpType,
			},
		})
	} else {
		// No worker — immediately mark as failed.
		cp.Status = loka.CheckpointStatusFailed
		m.store.Checkpoints().Create(ctx, cp)
		return cp, fmt.Errorf("no worker assigned to session")
	}

	return cp, nil
}

// CompleteCheckpoint updates a checkpoint after the worker finishes.
func (m *Manager) CompleteCheckpoint(ctx context.Context, checkpointID string, success bool, overlayKey, errMsg string) error {
	cp, err := m.store.Checkpoints().Get(ctx, checkpointID)
	if err != nil {
		return err
	}
	if success {
		cp.Status = loka.CheckpointStatusReady
		cp.OverlayPath = overlayKey
	} else {
		cp.Status = loka.CheckpointStatusFailed
	}
	// Update via delete + recreate since we don't have a dedicated Update method.
	m.store.Checkpoints().Delete(ctx, checkpointID)
	return m.store.Checkpoints().Create(ctx, cp)
}

// RestoreCheckpoint dispatches a checkpoint restore to the worker.
func (m *Manager) RestoreCheckpoint(ctx context.Context, sessionID, checkpointID string) error {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return err
	}
	cp, err := m.store.Checkpoints().Get(ctx, checkpointID)
	if err != nil {
		return err
	}
	if cp.Status != loka.CheckpointStatusReady {
		return fmt.Errorf("checkpoint %s is not ready (status: %s)", checkpointID, cp.Status)
	}
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "restore_checkpoint",
			Data: worker.RestoreCheckpointData{
				SessionID:    sessionID,
				CheckpointID: checkpointID,
				OverlayKey:   cp.OverlayPath,
			},
		})
	}
	m.logger.Info("checkpoint restore dispatched", "checkpoint", checkpointID, "session", sessionID)
	return nil
}

// GetExecution retrieves an execution by ID.
func (m *Manager) GetExecution(ctx context.Context, id string) (*loka.Execution, error) {
	return m.store.Executions().Get(ctx, id)
}

// ListExecutions returns executions for a session.
func (m *Manager) ListExecutions(ctx context.Context, sessionID string, filter store.ExecutionFilter) ([]*loka.Execution, error) {
	return m.store.Executions().ListBySession(ctx, sessionID, filter)
}
