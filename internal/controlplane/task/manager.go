// Package task manages one-time task execution in LOKA.
// Tasks reuse the service VM infrastructure but with restart_policy="never"
// and task-specific status semantics (pending/running/success/failed/error).
package task

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	cpworker "github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// RunOpts defines options for running a task.
type RunOpts struct {
	Name     string
	ImageRef string
	Command  string
	Args     []string
	Env      map[string]string
	Workdir  string
	VCPUs    int
	MemoryMB int
	Mounts   []loka.Volume
	Timeout  int // Max seconds (0 = no limit).
}

// Manager orchestrates task lifecycle.
type Manager struct {
	store     store.Store
	registry  *cpworker.Registry
	scheduler *scheduler.Scheduler
	images    *image.Manager
	logger    *slog.Logger
}

// NewManager creates a task manager.
// It resumes monitoring any tasks stuck in "running" or "pending" from a previous crash.
func NewManager(s store.Store, registry *cpworker.Registry, sched *scheduler.Scheduler, images *image.Manager, logger *slog.Logger) *Manager {
	m := &Manager{
		store:     s,
		registry:  registry,
		scheduler: sched,
		images:    images,
		logger:    logger,
	}
	m.recoverStuckTasks()
	return m
}

// recoverStuckTasks handles tasks left in non-terminal states after a CP restart.
// Running tasks older than 10 minutes are marked as errors since their monitoring
// goroutine is gone. Pending tasks are also marked as errors.
func (m *Manager) recoverStuckTasks() {
	ctx := context.Background()
	staleThreshold := time.Now().Add(-10 * time.Minute)

	for _, status := range []loka.TaskStatus{loka.TaskStatusRunning, loka.TaskStatusPending} {
		s := status
		tasks, err := m.store.Tasks().List(ctx, store.TaskFilter{Status: &s})
		if err != nil {
			m.logger.Warn("failed to check stuck tasks", "status", s, "error", err)
			continue
		}
		for _, task := range tasks {
			if task.CreatedAt.Before(staleThreshold) {
				task.Status = loka.TaskStatusError
				task.StatusMessage = "interrupted by restart"
				task.CompletedAt = time.Now()
				task.UpdatedAt = time.Now()
				if err := m.store.Tasks().Update(ctx, task); err != nil {
					m.logger.Warn("failed to recover stuck task", "id", task.ID, "error", err)
				} else {
					m.logger.Info("recovered stuck task", "id", task.ID, "name", task.Name, "was", s)
				}
			}
		}
	}
}

// Run creates and launches a task.
func (m *Manager) Run(ctx context.Context, opts RunOpts) (*loka.Task, error) {
	if opts.ImageRef == "" {
		return nil, fmt.Errorf("image is required")
	}
	if opts.Command == "" {
		return nil, fmt.Errorf("command is required")
	}

	now := time.Now()
	task := &loka.Task{
		ID:        uuid.New().String(),
		Name:      opts.Name,
		Status:    loka.TaskStatusPending,
		ImageRef:  opts.ImageRef,
		Command:   opts.Command,
		Args:      opts.Args,
		Env:       opts.Env,
		Workdir:   opts.Workdir,
		VCPUs:     opts.VCPUs,
		MemoryMB:  opts.MemoryMB,
		Mounts:    opts.Mounts,
		Timeout:   opts.Timeout,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if task.Name == "" {
		task.Name = "task-" + task.ID[:8]
	}
	if task.VCPUs == 0 {
		task.VCPUs = 1
	}
	if task.MemoryMB == 0 {
		task.MemoryMB = 512
	}

	if err := m.store.Tasks().Create(ctx, task); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	// Schedule to worker.
	wc, err := m.scheduler.Pick(scheduler.Constraints{})
	if err != nil {
		task.Status = loka.TaskStatusError
		task.StatusMessage = "no workers available"
		m.store.Tasks().Update(ctx, task)
		return task, fmt.Errorf("schedule: %w", err)
	}
	task.WorkerID = wc.Worker.ID

	// Launch async.
	go m.asyncRun(task)

	m.logger.Info("task created", "id", task.ID, "name", task.Name, "image", task.ImageRef)
	return task, nil
}

func (m *Manager) asyncRun(task *loka.Task) {
	ctx := context.Background()

	// Pull image.
	var rootfsPath string
	if m.images != nil {
		task.StatusMessage = "pulling image"
		m.store.Tasks().Update(ctx, task)

		img, err := m.images.Pull(ctx, task.ImageRef)
		if err != nil {
			task.Status = loka.TaskStatusError
			task.StatusMessage = fmt.Sprintf("image pull failed: %v", err)
			m.store.Tasks().Update(ctx, task)
			return
		}
		rp, err := m.images.ResolveRootfsPath(ctx, img.ID)
		if err == nil {
			rootfsPath = rp
		}
	}

	// Send launch command to worker.
	task.Status = loka.TaskStatusRunning
	task.StartedAt = time.Now()
	task.StatusMessage = "starting"
	m.store.Tasks().Update(ctx, task)

	launchData := cpworker.LaunchServiceData{
		ServiceID:     task.ID,
		ImageRef:      task.ImageRef,
		VCPUs:         task.VCPUs,
		MemoryMB:      task.MemoryMB,
		RootfsPath:    rootfsPath,
		Command:       task.Command,
		Args:          task.Args,
		Env:           task.Env,
		Workdir:       task.Workdir,
		Port:          0, // Tasks don't expose ports.
		RestartPolicy: "never",
		Mounts:        task.Mounts,
	}

	if err := m.registry.SendCommand(task.WorkerID, cpworker.WorkerCommand{
		ID:   task.ID,
		Type: "launch_service",
		Data: launchData,
	}); err != nil {
		task.Status = loka.TaskStatusError
		task.StatusMessage = fmt.Sprintf("launch failed: %v", err)
		m.store.Tasks().Update(ctx, task)
		return
	}

	task.StatusMessage = "running"
	m.store.Tasks().Update(ctx, task)

	// Monitor: poll service_status until process exits.
	m.monitorTask(ctx, task)
}

func (m *Manager) monitorTask(ctx context.Context, task *loka.Task) {
	timeout := time.Duration(task.Timeout) * time.Second
	if timeout == 0 {
		timeout = 1 * time.Hour // Default max: 1h.
	}
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			task.Status = loka.TaskStatusFailed
			task.ExitCode = -1
			task.StatusMessage = "timeout"
			task.CompletedAt = time.Now()
			m.store.Tasks().Update(ctx, task)
			// Stop the VM.
			m.registry.SendCommand(task.WorkerID, cpworker.WorkerCommand{
				Type: "stop_session",
				Data: map[string]string{"session_id": task.ID},
			})
			return
		}

		// Check service status via worker.
		wc, ok := m.registry.Get(task.WorkerID)
		if !ok {
			task.Status = loka.TaskStatusError
			task.StatusMessage = "worker disconnected"
			task.CompletedAt = time.Now()
			m.store.Tasks().Update(ctx, task)
			return
		}
		_ = wc // Status check would go through worker agent.

		// For now, poll the task record — the localworker updates it when the process exits.
		updated, err := m.store.Tasks().Get(ctx, task.ID)
		if err == nil && (updated.Status == loka.TaskStatusSuccess || updated.Status == loka.TaskStatusFailed || updated.Status == loka.TaskStatusError) {
			return // Task completed.
		}

		time.Sleep(2 * time.Second)
	}
}

// Get returns a task by ID or name.
func (m *Manager) Get(ctx context.Context, id string) (*loka.Task, error) {
	return m.store.Tasks().Get(ctx, id)
}

// List returns all tasks matching the filter.
func (m *Manager) List(ctx context.Context, filter store.TaskFilter) ([]*loka.Task, error) {
	return m.store.Tasks().List(ctx, filter)
}

// Restart re-runs a completed/failed task.
func (m *Manager) Restart(ctx context.Context, id string) (*loka.Task, error) {
	task, err := m.store.Tasks().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("task not found: %w", err)
	}
	if task.Status == loka.TaskStatusRunning || task.Status == loka.TaskStatusPending {
		return nil, fmt.Errorf("task is already %s", task.Status)
	}

	// Create a new task with the same config.
	return m.Run(ctx, RunOpts{
		Name:     task.Name,
		ImageRef: task.ImageRef,
		Command:  task.Command,
		Args:     task.Args,
		Env:      task.Env,
		Workdir:  task.Workdir,
		VCPUs:    task.VCPUs,
		MemoryMB: task.MemoryMB,
		Mounts:   task.Mounts,
		Timeout:  task.Timeout,
	})
}

// Cancel stops a running task.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	task, err := m.store.Tasks().Get(ctx, id)
	if err != nil {
		return fmt.Errorf("task not found: %w", err)
	}
	if task.Status != loka.TaskStatusRunning && task.Status != loka.TaskStatusPending {
		return fmt.Errorf("task is %s, cannot cancel", task.Status)
	}

	// Stop the VM.
	if task.WorkerID != "" {
		m.registry.SendCommand(task.WorkerID, cpworker.WorkerCommand{
			Type: "stop_session",
			Data: map[string]string{"session_id": task.ID},
		})
	}

	task.Status = loka.TaskStatusFailed
	task.ExitCode = -1
	task.StatusMessage = "cancelled"
	task.CompletedAt = time.Now()
	return m.store.Tasks().Update(ctx, task)
}

// Delete removes a task record.
func (m *Manager) Delete(ctx context.Context, id string) error {
	task, err := m.store.Tasks().Get(ctx, id)
	if err != nil {
		return fmt.Errorf("task not found: %w", err)
	}
	if task.Status == loka.TaskStatusRunning {
		m.Cancel(ctx, id)
	}
	return m.store.Tasks().Delete(ctx, id)
}
