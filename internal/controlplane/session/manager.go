package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/metrics"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
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
	WorkerLabels map[string]string    // Scheduling affinity labels.
	ExecPolicy   *loka.ExecPolicy     // Command restrictions. Nil = default policy.
	Mounts       []loka.StorageMount  // Object storage mounts.
	Ports        []loka.PortMapping   // Port forwarding declarations.
	IdleTimeout  int                  // Seconds before auto-idle (0 = never).
}

const artifactBucket = "sessions"

// Manager orchestrates session lifecycle.
type Manager struct {
	store     store.Store
	registry  *worker.Registry
	scheduler *scheduler.Scheduler
	images    *image.Manager
	objStore  objstore.ObjectStore
	logger    *slog.Logger
}

// NewManager creates a new session manager.
func NewManager(s store.Store, reg *worker.Registry, sched *scheduler.Scheduler, imgMgr *image.Manager, objStore objstore.ObjectStore, logger *slog.Logger) *Manager {
	return &Manager{store: s, registry: reg, scheduler: sched, images: imgMgr, objStore: objStore, logger: logger}
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
		Mounts:     opts.Mounts,
		Ports:       opts.Ports,
		ExecPolicy:  execPolicy,
		IdleTimeout: opts.IdleTimeout,
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
		s.Ready = true
	} else {
		s.WorkerID = wConn.Worker.ID

		// Check if image is cached — if not, we go to provisioning first.
		imageReady := false
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
				rootfs, err := m.images.ResolveRootfsPath(ctx, img.ID)
				if err != nil {
					m.logger.Error("resolve rootfs path failed", "image", img.ID, "error", err)
				} else {
					launchData.RootfsPath = rootfs
				}
				launchData.SnapshotMemPath = img.SnapshotMem
				launchData.SnapshotVMStatePath = img.SnapshotVMState
				imageReady = true
			}
		}

		if imageReady || s.ImageRef == "" || m.images == nil {
			// Image cached — go straight to running.
			s.Status = loka.SessionStatusRunning
			s.Ready = true
		} else {
			// Image needs pulling — go to provisioning.
			// The pull happens asynchronously; MarkReady is called when done.
			s.Status = loka.SessionStatusProvisioning
			s.StatusMessage = "pulling image"

			// Async: pull image, then mark session running.
			go func() {
				pullCtx := context.Background()
				m.logger.Info("pulling image for session", "session", s.ID, "image", s.ImageRef)

				m.UpdateStatusMessage(pullCtx, s.ID, "pulling image")
				img, err := m.images.Pull(pullCtx, s.ImageRef)
				if err != nil {
					m.logger.Error("image pull failed", "session", s.ID, "error", err)
					m.UpdateStatusMessage(pullCtx, s.ID, "image pull failed: "+err.Error())
					// Set session to error.
					if ss, e := m.store.Sessions().Get(pullCtx, s.ID); e == nil {
						ss.Status = loka.SessionStatusError
						ss.StatusMessage = "image pull failed: " + err.Error()
						m.store.Sessions().Update(pullCtx, ss)
					}
					return
				}

				// Update launch data with image paths.
				rootfs, resolveErr := m.images.ResolveRootfsPath(pullCtx, img.ID)
				if resolveErr != nil {
					m.logger.Error("resolve rootfs path failed", "session", s.ID, "error", resolveErr)
					return
				}
				launchData.RootfsPath = rootfs
				launchData.SnapshotMemPath = img.SnapshotMem
				launchData.SnapshotVMStatePath = img.SnapshotVMState

				m.UpdateStatusMessage(pullCtx, s.ID, "booting")

				// Send launch command to worker.
				m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
					ID:   uuid.New().String(),
					Type: "launch_session",
					Data: launchData,
				})

				// Mark session as running.
				m.MarkReady(pullCtx, s.ID)
				m.logger.Info("session provisioned", "session", s.ID, "image", s.ImageRef)
			}()
		}

		// Only send launch command immediately if the image is already cached.
		// Provisioning sessions send it after the image pull completes (in the goroutine above).
		if imageReady || s.ImageRef == "" || m.images == nil {
			m.registry.SendCommand(wConn.Worker.ID, worker.WorkerCommand{
				ID:   uuid.New().String(),
				Type: "launch_session",
				Data: launchData,
			})
		}
		m.logger.Info("session scheduled to worker", "session", s.ID, "worker", wConn.Worker.ID, "image_ready", imageReady)
	}

	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, fmt.Errorf("update session status: %w", err)
	}

	return s, nil
}

// MarkReady is called by the worker when the supervisor confirms the VM is alive.
func (m *Manager) MarkReady(ctx context.Context, sessionID string) error {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return err
	}
	s.Ready = true
	s.Status = loka.SessionStatusRunning
	s.StatusMessage = ""
	s.LastActivity = time.Now()
	s.UpdatedAt = time.Now()
	return m.store.Sessions().Update(ctx, s)
}

// UpdateStatusMessage updates the provisioning progress message.
func (m *Manager) UpdateStatusMessage(ctx context.Context, sessionID, message string) error {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return err
	}
	s.StatusMessage = message
	s.UpdatedAt = time.Now()
	return m.store.Sessions().Update(ctx, s)
}

// WaitForReady blocks until the session is ready or the context is cancelled.
func (m *Manager) WaitForReady(ctx context.Context, sessionID string) (*loka.Session, error) {
	for {
		s, err := m.store.Sessions().Get(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if s.Ready || s.Status == loka.SessionStatusRunning {
			return s, nil
		}
		if s.Status == loka.SessionStatusError {
			msg := s.StatusMessage
			if msg == "" {
				msg = "session failed"
			}
			return s, fmt.Errorf("%s", msg)
		}
		if s.Status == loka.SessionStatusTerminated {
			return s, fmt.Errorf("session terminated before becoming ready")
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
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

// Purge fully removes a session and all associated data (checkpoints,
// executions) from the database and sends a cleanup command to the worker.
func (m *Manager) Purge(ctx context.Context, id string) error {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return err
	}

	// Send cleanup command to worker if assigned.
	if s.WorkerID != "" {
		m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "cleanup_session",
			Data: worker.CleanupSessionData{SessionID: s.ID},
		})
	}

	// Delete all checkpoints for this session.
	cps, err := m.store.Checkpoints().ListBySession(ctx, id)
	if err != nil {
		m.logger.Warn("failed to list checkpoints for purge", "session", id, "error", err)
	} else {
		for _, cp := range cps {
			if err := m.store.Checkpoints().Delete(ctx, cp.ID); err != nil {
				m.logger.Warn("failed to delete checkpoint during purge", "checkpoint", cp.ID, "error", err)
			}
		}
	}

	// Delete all executions for this session.
	execs, err := m.store.Executions().ListBySession(ctx, id, store.ExecutionFilter{})
	if err != nil {
		m.logger.Warn("failed to list executions for purge", "session", id, "error", err)
	} else {
		for _, exec := range execs {
			exec.Status = "purged"
			if err := m.store.Executions().Update(ctx, exec); err != nil {
				m.logger.Warn("failed to update execution during purge", "exec", exec.ID, "error", err)
			}
		}
	}

	// Delete the session from the database.
	if err := m.store.Sessions().Delete(ctx, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	metrics.SessionsDestroyed.Inc()
	m.logger.Info("session purged", "id", id)
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

// Idle puts a running session into idle state — the VM is suspended to save
// resources but can be auto-warmed when accessed (exec, port-forward, sync).
func (m *Manager) Idle(ctx context.Context, id string) (*loka.Session, error) {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.CanTransitionTo(loka.SessionStatusIdle) {
		return nil, fmt.Errorf("cannot idle session in status %s", s.Status)
	}

	// Tell worker to suspend the VM (snapshot memory + pause).
	if s.WorkerID != "" {
		if wc, ok := m.registry.Get(s.WorkerID); ok {
			wc.CmdChan <- worker.WorkerCommand{Type: "pause_session", Data: worker.StopSessionData{SessionID: id}}
		}
	}

	s.Status = loka.SessionStatusIdle
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, err
	}
	m.logger.Info("session idled", "session", id)
	return s, nil
}

// Wake brings an idle session back to running. Called automatically when the
// session is accessed (exec, port-forward, sync, domain proxy).
func (m *Manager) Wake(ctx context.Context, id string) (*loka.Session, error) {
	s, err := m.store.Sessions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.Status != loka.SessionStatusIdle {
		if s.Status == loka.SessionStatusRunning {
			return s, nil // Already running.
		}
		return nil, fmt.Errorf("cannot wake session in status %s", s.Status)
	}

	// Tell worker to resume the VM.
	if s.WorkerID != "" {
		if wc, ok := m.registry.Get(s.WorkerID); ok {
			wc.CmdChan <- worker.WorkerCommand{Type: "resume_session", Data: worker.LaunchSessionData{SessionID: id}}
		}
	}

	s.Status = loka.SessionStatusRunning
	s.LastActivity = time.Now()
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return nil, err
	}
	m.logger.Info("session woken from idle", "session", id)
	return s, nil
}

// ensureRunning wakes an idle session if needed. Returns error if session
// is not running and cannot be woken.
func (m *Manager) ensureRunning(ctx context.Context, s *loka.Session) (*loka.Session, error) {
	if s.Status == loka.SessionStatusRunning {
		return s, nil
	}
	if s.Status == loka.SessionStatusIdle {
		return m.Wake(ctx, s.ID)
	}
	return nil, fmt.Errorf("session is %s, must be running", s.Status)
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
	// Auto-wake idle sessions.
	s, err = m.ensureRunning(ctx, s)
	if err != nil {
		return nil, err
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

// SyncMount syncs data between a session's storage mount and the object store.
// The actual sync is dispatched to the worker running the session's VM.
func (m *Manager) SyncMount(ctx context.Context, sessionID string, req loka.SyncRequest) (*loka.SyncResult, error) {
	sess, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}
	// Auto-wake idle sessions.
	sess, err = m.ensureRunning(ctx, sess)
	if err != nil {
		return nil, err
	}

	if sess.WorkerID == "" {
		return nil, fmt.Errorf("session has no assigned worker")
	}

	// Find the worker and dispatch the sync command.
	wc, ok := m.registry.Get(sess.WorkerID)
	if !ok {
		return nil, fmt.Errorf("worker %s not available", sess.WorkerID)
	}

	// Send sync command to worker.
	wc.CmdChan <- worker.WorkerCommand{
		Type: "sync_mount",
		Data: worker.SyncMountData{
			SessionID: sessionID,
			MountPath: req.MountPath,
			Direction: string(req.Direction),
			Prefix:    req.Prefix,
			Delete:    req.Delete,
			DryRun:    req.DryRun,
		},
	}

	// For now, return a placeholder result.
	// In production, the worker would report back via the exec/report channel.
	return &loka.SyncResult{
		MountPath: req.MountPath,
		Direction: string(req.Direction),
	}, nil
}

// ListArtifacts returns files changed in a session relative to the base image.
// If checkpointID is empty, returns the live diff from the worker.
// If checkpointID is set, returns the diff stored with that checkpoint.
func (m *Manager) ListArtifacts(ctx context.Context, sessionID, checkpointID string) ([]*loka.Artifact, error) {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	if checkpointID != "" {
		// Return artifacts stored with the checkpoint.
		cp, err := m.store.Checkpoints().Get(ctx, checkpointID)
		if err != nil {
			return nil, fmt.Errorf("checkpoint not found: %w", err)
		}
		if cp.SessionID != sessionID {
			return nil, fmt.Errorf("checkpoint %s does not belong to session %s", checkpointID, sessionID)
		}
		// Read manifest from objstore. If not found, return empty list (no artifacts uploaded yet).
		key := fmt.Sprintf("%s/checkpoints/%s/artifacts/manifest.json", sessionID, checkpointID)
		arts, err := m.readArtifactManifest(ctx, key)
		if err != nil {
			return []*loka.Artifact{}, nil
		}
		return arts, nil
	}

	// Live diff: try the worker first, fall back to objstore manifest.
	s, err = m.ensureRunning(ctx, s)
	if err != nil {
		// Session not running — try reading persisted manifest from objstore.
		key := fmt.Sprintf("%s/artifacts/manifest.json", sessionID)
		arts, objErr := m.readArtifactManifest(ctx, key)
		if objErr == nil {
			return arts, nil
		}
		return nil, err
	}

	if s.WorkerID == "" {
		// No worker — try objstore fallback.
		key := fmt.Sprintf("%s/artifacts/manifest.json", sessionID)
		arts, objErr := m.readArtifactManifest(ctx, key)
		if objErr == nil {
			return arts, nil
		}
		return nil, fmt.Errorf("session has no assigned worker")
	}

	// Send list_artifacts command to the worker.
	m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "list_artifacts",
		Data: map[string]string{"session_id": sessionID},
	})

	// Also try objstore as fallback for immediate response.
	key := fmt.Sprintf("%s/artifacts/manifest.json", sessionID)
	arts, objErr := m.readArtifactManifest(ctx, key)
	if objErr == nil && len(arts) > 0 {
		return arts, nil
	}

	m.logger.Info("list_artifacts dispatched", "session", sessionID, "worker", s.WorkerID)
	return []*loka.Artifact{}, nil
}

// readArtifactManifest reads and decodes an artifact manifest from the object store.
func (m *Manager) readArtifactManifest(ctx context.Context, key string) ([]*loka.Artifact, error) {
	if m.objStore == nil {
		return nil, fmt.Errorf("object store not configured")
	}
	reader, err := m.objStore.Get(ctx, artifactBucket, key)
	if err != nil {
		return nil, fmt.Errorf("manifest not found in objstore: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	var arts []*loka.Artifact
	if err := json.Unmarshal(data, &arts); err != nil {
		return nil, fmt.Errorf("failed to decode manifest: %w", err)
	}
	return arts, nil
}

// DownloadArtifact reads a single file from the session's overlay.
// It first tries the object store, falling back to the worker if not found.
func (m *Manager) DownloadArtifact(ctx context.Context, sessionID, path string) ([]byte, error) {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	// Try reading from objstore first.
	if m.objStore != nil {
		pathHash := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
		key := fmt.Sprintf("%s/artifacts/%s.dat", sessionID, pathHash)
		reader, objErr := m.objStore.Get(ctx, artifactBucket, key)
		if objErr == nil {
			defer reader.Close()
			data, err := io.ReadAll(reader)
			if err != nil {
				return nil, fmt.Errorf("failed to read artifact from objstore: %w", err)
			}
			return data, nil
		}
	}

	// Fall back to worker dispatch.
	s, err = m.ensureRunning(ctx, s)
	if err != nil {
		return nil, err
	}

	if s.WorkerID == "" {
		return nil, fmt.Errorf("session has no assigned worker and artifact not in object store")
	}

	// Send read_file command to the worker.
	m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "read_file",
		Data: map[string]string{"session_id": sessionID, "path": path},
	})

	m.logger.Info("read_file dispatched", "session", sessionID, "path", path, "worker", s.WorkerID)
	return []byte{}, nil
}

// DownloadArtifactsTar streams all changed files as a tar archive.
func (m *Manager) DownloadArtifactsTar(ctx context.Context, sessionID string) ([]byte, error) {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	s, err = m.ensureRunning(ctx, s)
	if err != nil {
		return nil, err
	}

	if s.WorkerID == "" {
		return nil, fmt.Errorf("session has no assigned worker")
	}

	// Send download_artifacts_tar command to the worker.
	m.registry.SendCommand(s.WorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "download_artifacts_tar",
		Data: map[string]string{"session_id": sessionID},
	})

	// In production, the worker would stream the tar archive back.
	// For now, return a placeholder.
	m.logger.Info("download_artifacts_tar dispatched", "session", sessionID, "worker", s.WorkerID)
	return []byte{}, nil
}

// UploadArtifacts persists artifact files and manifest to the object store.
// Called by the worker after exec completes to store changed files.
func (m *Manager) UploadArtifacts(ctx context.Context, sessionID string, artifacts []loka.Artifact, files map[string][]byte) error {
	if m.objStore == nil {
		return fmt.Errorf("object store not configured")
	}
	// Upload each file.
	for path, data := range files {
		pathHash := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
		key := fmt.Sprintf("%s/artifacts/%s.dat", sessionID, pathHash)
		if err := m.objStore.Put(ctx, artifactBucket, key, bytes.NewReader(data), int64(len(data))); err != nil {
			return fmt.Errorf("failed to upload artifact %s: %w", path, err)
		}
		m.logger.Debug("uploaded artifact", "session", sessionID, "path", path, "key", key, "size", len(data))
	}

	// Write manifest.json with the artifact list.
	manifestData, err := json.Marshal(artifacts)
	if err != nil {
		return fmt.Errorf("failed to marshal artifact manifest: %w", err)
	}
	manifestKey := fmt.Sprintf("%s/artifacts/manifest.json", sessionID)
	if err := m.objStore.Put(ctx, artifactBucket, manifestKey, bytes.NewReader(manifestData), int64(len(manifestData))); err != nil {
		return fmt.Errorf("failed to upload artifact manifest: %w", err)
	}

	m.logger.Info("artifacts uploaded", "session", sessionID, "count", len(artifacts), "files", len(files))
	return nil
}
