package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
)

// DeployOpts holds options for deploying a new service.
type DeployOpts struct {
	Name           string
	ImageRef       string
	RecipeName     string
	Command        string
	Args           []string
	Env            map[string]string
	Workdir        string
	Port           int
	VCPUs          int
	MemoryMB       int
	Routes         []loka.ServiceRoute
	BundleKey      string // objstore key for uploaded bundle
	IdleTimeout    int
	HealthPath     string
	HealthInterval int
	HealthTimeout  int
	HealthRetries  int
	Labels         map[string]string
	Mounts         []loka.VolumeMount
	Autoscale      *loka.AutoscaleConfig
}

// Manager orchestrates service lifecycle.
type Manager struct {
	store     store.Store
	registry  *worker.Registry
	scheduler *scheduler.Scheduler
	images    *image.Manager
	objStore  objstore.ObjectStore
	logger    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	logsFn func(serviceID string, lines int) ([]string, []string, error) // set by localworker
}

// NewManager creates a new service manager.
func NewManager(s store.Store, reg *worker.Registry, sched *scheduler.Scheduler, imgMgr *image.Manager, objStore objstore.ObjectStore, logger *slog.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		store:     s,
		registry:  reg,
		scheduler: sched,
		images:    imgMgr,
		objStore:  objStore,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
	}
	m.wg.Add(1)
	go m.idleMonitor()
	return m
}

// Close cancels all in-flight goroutines and waits for them to finish.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// Deploy creates a new service record and schedules it to a worker.
func (m *Manager) Deploy(ctx context.Context, opts DeployOpts) (*loka.Service, error) {
	if opts.VCPUs == 0 {
		opts.VCPUs = 1
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = 512
	}
	if opts.Port == 0 {
		opts.Port = 8080
	}
	if opts.Labels == nil {
		opts.Labels = make(map[string]string)
	}
	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}
	if opts.HealthPath == "" {
		opts.HealthPath = "/health"
	}
	if opts.HealthInterval == 0 {
		opts.HealthInterval = 10
	}
	if opts.HealthTimeout == 0 {
		opts.HealthTimeout = 5
	}
	if opts.HealthRetries == 0 {
		opts.HealthRetries = 3
	}

	now := time.Now()
	svc := &loka.Service{
		ID:             uuid.New().String(),
		Name:           opts.Name,
		Status:         loka.ServiceStatusDeploying,
		ImageRef:       opts.ImageRef,
		RecipeName:     opts.RecipeName,
		Command:        opts.Command,
		Args:           opts.Args,
		Env:            opts.Env,
		Workdir:        opts.Workdir,
		Port:           opts.Port,
		VCPUs:          opts.VCPUs,
		MemoryMB:       opts.MemoryMB,
		Routes:         opts.Routes,
		BundleKey:      opts.BundleKey,
		IdleTimeout:    opts.IdleTimeout,
		HealthPath:     opts.HealthPath,
		HealthInterval: opts.HealthInterval,
		HealthTimeout:  opts.HealthTimeout,
		HealthRetries:  opts.HealthRetries,
		Labels:         opts.Labels,
		Mounts:         opts.Mounts,
		Autoscale:      opts.Autoscale,
		LastActivity:   now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := m.store.Services().Create(ctx, svc); err != nil {
		return nil, fmt.Errorf("create service: %w", err)
	}

	m.logger.Info("service created", "id", svc.ID, "name", svc.Name)

	// Schedule to a worker via scheduler.
	wConn, err := m.scheduler.Pick(scheduler.Constraints{})
	if err != nil {
		// No workers available — mark deploying, will retry when a worker appears.
		m.logger.Warn("no workers available for service", "id", svc.ID)
		svc.StatusMessage = "waiting for worker"
		svc.UpdatedAt = time.Now()
		m.store.Services().Update(ctx, svc)
		return svc, nil
	}

	svc.WorkerID = wConn.Worker.ID
	svc.UpdatedAt = time.Now()
	m.store.Services().Update(ctx, svc)

	// Launch the async deploy goroutine.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.asyncDeploy(m.ctx, svc.ID, opts)
	}()

	m.logger.Info("service scheduled to worker", "service", svc.ID, "worker", wConn.Worker.ID)
	return svc, nil
}

// asyncDeploy handles image pulling, sending launch command, and health checking.
func (m *Manager) asyncDeploy(ctx context.Context, serviceID string, opts DeployOpts) {
	svc, err := m.store.Services().Get(ctx, serviceID)
	if err != nil {
		m.logger.Error("async deploy: failed to get service", "service", serviceID, "error", err)
		return
	}

	// 1. Pull image if needed.
	var rootfsPath string
	if svc.ImageRef != "" && m.images != nil {
		m.updateStatusMessage(ctx, serviceID, "pulling image")

		// Check if image is already cached.
		if img, ok := m.images.GetByRef(svc.ImageRef); ok {
			svc.ImageID = img.ID
			rp, resolveErr := m.images.ResolveRootfsPath(ctx, img.ID)
			if resolveErr != nil {
				m.logger.Error("resolve rootfs path failed", "service", serviceID, "error", resolveErr)
			} else {
				rootfsPath = rp
			}
		} else {
			img, pullErr := m.images.Pull(ctx, svc.ImageRef)
			if pullErr != nil {
				m.logger.Error("image pull failed for service", "service", serviceID, "error", pullErr)
				m.setError(ctx, serviceID, "image pull failed: "+pullErr.Error())
				return
			}
			svc.ImageID = img.ID
			rp, resolveErr := m.images.ResolveRootfsPath(ctx, img.ID)
			if resolveErr != nil {
				m.logger.Error("resolve rootfs path failed", "service", serviceID, "error", resolveErr)
			} else {
				rootfsPath = rp
			}
		}
	}

	// 2. Send launch_service command to worker.
	m.updateStatusMessage(ctx, serviceID, "launching")

	launchData := worker.LaunchServiceData{
		ServiceID:  serviceID,
		ImageRef:   svc.ImageRef,
		VCPUs:      svc.VCPUs,
		MemoryMB:   svc.MemoryMB,
		RootfsPath: rootfsPath,
		Command:    svc.Command,
		Args:       svc.Args,
		Env:        svc.Env,
		Workdir:    svc.Workdir,
		Port:       svc.Port,
		BundleKey:  svc.BundleKey,
		Mounts:     svc.Mounts,
	}

	m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "launch_service",
		Data: launchData,
	})

	// 3. Wait for health check to pass (polling with timeout).
	// TODO: Implement actual HTTP health checking by sending an exec command
	// to the worker to run: wget -q -O /dev/null http://localhost:{port}{health_path}
	// For now, we poll service_status and assume healthy after successful status checks.
	m.updateStatusMessage(ctx, serviceID, "waiting for health check")

	// Use the service's configured health parameters.
	healthInterval := time.Duration(svc.HealthInterval) * time.Second
	if healthInterval <= 0 {
		healthInterval = 5 * time.Second
	}
	healthTimeout := time.Duration(svc.HealthTimeout) * time.Second
	if healthTimeout <= 0 {
		healthTimeout = 60 * time.Second
	}
	healthRetries := svc.HealthRetries
	if healthRetries <= 0 {
		healthRetries = 12
	}

	deadline := time.After(healthTimeout)
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()

	for retries := 0; retries < healthRetries; retries++ {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			m.setError(ctx, serviceID, "health check timeout")
			return
		case <-ticker.C:
			// Poll service status by checking the service record.
			s, err := m.store.Services().Get(ctx, serviceID)
			if err != nil {
				m.logger.Error("async deploy: health check poll failed", "service", serviceID, "error", err)
				continue
			}
			if s.Status != loka.ServiceStatusDeploying {
				// Status changed externally (e.g., destroyed or errored), do not override.
				return
			}

			// Send a service_status command to the worker to check if the process is running.
			m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
				ID:   uuid.New().String(),
				Type: "service_status",
				Data: map[string]string{"service_id": serviceID},
			})

			// After the first successful poll interval, the launch command has had
			// time to start the process. Mark the service as running.
			if retries >= 1 {
				goto healthy
			}
		}
	}
	m.setError(ctx, serviceID, "health check exhausted retries")
	return

healthy:
	// 4. Update status to running.
	svc, err = m.store.Services().Get(ctx, serviceID)
	if err != nil {
		m.logger.Error("async deploy: failed to refresh service", "service", serviceID, "error", err)
		return
	}
	if svc.Status != loka.ServiceStatusDeploying {
		// Status changed externally (e.g., destroyed), do not override.
		return
	}

	svc.Status = loka.ServiceStatusRunning
	svc.Ready = true
	svc.StatusMessage = ""
	svc.LastActivity = time.Now()
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		m.logger.Error("async deploy: failed to mark service running", "service", serviceID, "error", err)
		return
	}

	m.logger.Info("service deployed and running", "service", serviceID)
}

// Get retrieves a service by ID.
func (m *Manager) Get(ctx context.Context, id string) (*loka.Service, error) {
	return m.store.Services().Get(ctx, id)
}

// List returns services matching the filter.
func (m *Manager) List(ctx context.Context, filter store.ServiceFilter) ([]*loka.Service, int, error) {
	return m.store.Services().List(ctx, filter)
}

// Destroy terminates a service and removes its record.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return err
	}

	// Send stop command to worker if assigned.
	if svc.WorkerID != "" {
		m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "stop_service",
			Data: worker.StopServiceData{ServiceID: svc.ID},
		})
	}

	if err := m.store.Services().Delete(ctx, id); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	m.logger.Info("service destroyed", "id", id)
	return nil
}

// Stop stops the service process but keeps the record.
func (m *Manager) Stop(ctx context.Context, id string) (*loka.Service, error) {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !svc.CanTransitionTo(loka.ServiceStatusStopped) {
		return nil, fmt.Errorf("cannot stop service in status %s", svc.Status)
	}

	if svc.WorkerID != "" {
		m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "stop_service",
			Data: worker.StopServiceData{ServiceID: svc.ID},
		})
	}

	svc.Status = loka.ServiceStatusStopped
	svc.Ready = false
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		return nil, err
	}
	m.logger.Info("service stopped", "id", id)
	return svc, nil
}

// Redeploy stops the service, re-extracts the bundle, and restarts it.
func (m *Manager) Redeploy(ctx context.Context, id string) (*loka.Service, error) {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// Stop first if running.
	if svc.Status == loka.ServiceStatusRunning || svc.Status == loka.ServiceStatusIdle {
		if svc.WorkerID != "" {
			m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
				ID:   uuid.New().String(),
				Type: "stop_service",
				Data: worker.StopServiceData{ServiceID: svc.ID},
			})
		}
	}

	// Reset to deploying and re-launch.
	svc.Status = loka.ServiceStatusDeploying
	svc.Ready = false
	svc.StatusMessage = "redeploying"
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		return nil, err
	}

	// Re-launch async deploy.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.asyncDeploy(m.ctx, svc.ID, DeployOpts{
			Name:           svc.Name,
			ImageRef:       svc.ImageRef,
			RecipeName:     svc.RecipeName,
			Command:        svc.Command,
			Args:           svc.Args,
			Env:            svc.Env,
			Workdir:        svc.Workdir,
			Port:           svc.Port,
			VCPUs:          svc.VCPUs,
			MemoryMB:       svc.MemoryMB,
			Routes:         svc.Routes,
			BundleKey:      svc.BundleKey,
			IdleTimeout:    svc.IdleTimeout,
			HealthPath:     svc.HealthPath,
			HealthInterval: svc.HealthInterval,
			HealthTimeout:  svc.HealthTimeout,
			HealthRetries:  svc.HealthRetries,
			Labels:         svc.Labels,
			Mounts:         svc.Mounts,
			Autoscale:      svc.Autoscale,
		})
	}()

	m.logger.Info("service redeploying", "id", id)
	return svc, nil
}

// UpdateEnv updates the service environment variables and restarts.
func (m *Manager) UpdateEnv(ctx context.Context, id string, env map[string]string) (*loka.Service, error) {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return nil, err
	}

	svc.Env = env
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		return nil, err
	}

	// Restart the service if it is running.
	if svc.Status == loka.ServiceStatusRunning || svc.Status == loka.ServiceStatusIdle {
		return m.Redeploy(ctx, id)
	}

	return svc, nil
}

// Wake resumes a service from idle state.
func (m *Manager) Wake(ctx context.Context, id string) (*loka.Service, error) {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if svc.Status != loka.ServiceStatusIdle {
		if svc.Status == loka.ServiceStatusRunning {
			return svc, nil // Already running.
		}
		return nil, fmt.Errorf("cannot wake service in status %s", svc.Status)
	}

	svc.Status = loka.ServiceStatusWaking
	svc.StatusMessage = "waking"
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		return nil, err
	}

	// Send resume command to worker.
	if svc.WorkerID != "" {
		m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "launch_service",
			Data: worker.LaunchServiceData{
				ServiceID: svc.ID,
				ImageRef:  svc.ImageRef,
				VCPUs:     svc.VCPUs,
				MemoryMB:  svc.MemoryMB,
				Command:   svc.Command,
				Args:      svc.Args,
				Env:       svc.Env,
				Workdir:   svc.Workdir,
				Port:      svc.Port,
				BundleKey: svc.BundleKey,
			},
		})
	}

	// Poll for the service to become ready after wake.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		const wakeTimeout = 60 * time.Second
		const pollInterval = 5 * time.Second
		const maxRetries = 12

		deadline := time.After(wakeTimeout)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for retries := 0; retries < maxRetries; retries++ {
			select {
			case <-m.ctx.Done():
				return
			case <-deadline:
				m.setError(m.ctx, id, "wake health check timeout")
				return
			case <-ticker.C:
				s, err := m.store.Services().Get(m.ctx, id)
				if err != nil || s.Status != loka.ServiceStatusWaking {
					return
				}

				// Send a status check to the worker.
				if svc.WorkerID != "" {
					m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
						ID:   uuid.New().String(),
						Type: "service_status",
						Data: map[string]string{"service_id": id},
					})
				}

				// After the first poll interval, assume the process has started.
				if retries >= 1 {
					s.Status = loka.ServiceStatusRunning
					s.Ready = true
					s.StatusMessage = ""
					s.LastActivity = time.Now()
					s.UpdatedAt = time.Now()
					m.store.Services().Update(m.ctx, s)
					m.logger.Info("service woken from idle", "service", id)
					return
				}
			}
		}
	}()

	return svc, nil
}

// Touch updates the LastActivity timestamp for a service.
func (m *Manager) Touch(ctx context.Context, id string) error {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return err
	}
	svc.LastActivity = time.Now()
	svc.UpdatedAt = time.Now()
	return m.store.Services().Update(ctx, svc)
}

// SetLogsFn sets the callback used to retrieve service logs.
// This is typically called by the local worker to wire up direct agent access.
func (m *Manager) SetLogsFn(fn func(string, int) ([]string, []string, error)) {
	m.logsFn = fn
}

// Logs retrieves recent log lines from the worker for a service.
func (m *Manager) Logs(ctx context.Context, id string, lines int) (stdout []string, stderr []string, err error) {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	if svc.Status != loka.ServiceStatusRunning {
		return nil, nil, fmt.Errorf("service not running (status: %s)", svc.Status)
	}

	if m.logsFn != nil {
		return m.logsFn(id, lines)
	}

	return nil, nil, fmt.Errorf("log retrieval not available")
}

// idleMonitor periodically checks running services and transitions idle ones.
func (m *Manager) idleMonitor() {
	defer m.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkIdleServices()
		}
	}
}

// checkIdleServices finds running services that have exceeded their idle timeout
// and transitions them to idle status.
func (m *Manager) checkIdleServices() {
	running := loka.ServiceStatusRunning
	services, _, err := m.store.Services().List(m.ctx, store.ServiceFilter{Status: &running})
	if err != nil {
		m.logger.Error("idle monitor: failed to list services", "error", err)
		return
	}
	for _, svc := range services {
		if svc.IdleTimeout <= 0 {
			continue // never idle
		}
		if time.Since(svc.LastActivity) > time.Duration(svc.IdleTimeout)*time.Second {
			m.logger.Info("service idle, transitioning", "service_id", svc.ID, "name", svc.Name)
			// Send stop command to worker (stops process, keeps VM state).
			if svc.WorkerID != "" {
				m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
					Type: "stop_service",
					Data: worker.StopServiceData{ServiceID: svc.ID},
				})
			}
			svc.Status = loka.ServiceStatusIdle
			svc.Ready = false
			svc.UpdatedAt = time.Now()
			if err := m.store.Services().Update(m.ctx, svc); err != nil {
				m.logger.Error("idle monitor: failed to update service", "service_id", svc.ID, "error", err)
			}
		}
	}
}

// WaitForReady blocks until the service is ready or the context is cancelled.
func (m *Manager) WaitForReady(ctx context.Context, serviceID string) (*loka.Service, error) {
	for {
		svc, err := m.store.Services().Get(ctx, serviceID)
		if err != nil {
			return nil, err
		}
		if svc.Ready || svc.Status == loka.ServiceStatusRunning {
			return svc, nil
		}
		if svc.Status == loka.ServiceStatusError {
			msg := svc.StatusMessage
			if msg == "" {
				msg = "service deployment failed"
			}
			return svc, fmt.Errorf("%s", msg)
		}
		if svc.Status == loka.ServiceStatusStopped {
			return svc, fmt.Errorf("service stopped before becoming ready")
		}
		select {
		case <-ctx.Done():
			return svc, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// updateStatusMessage updates the status message of a service.
func (m *Manager) updateStatusMessage(ctx context.Context, serviceID, message string) {
	svc, err := m.store.Services().Get(ctx, serviceID)
	if err != nil {
		m.logger.Error("failed to get service for status update", "service_id", serviceID, "error", err)
		return
	}
	svc.StatusMessage = message
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		m.logger.Error("failed to update service status message", "service_id", serviceID, "error", err)
	}
}

// setError marks a service as errored.
func (m *Manager) setError(ctx context.Context, serviceID, message string) {
	svc, err := m.store.Services().Get(ctx, serviceID)
	if err != nil {
		return
	}
	svc.Status = loka.ServiceStatusError
	svc.StatusMessage = message
	svc.Ready = false
	svc.UpdatedAt = time.Now()
	m.store.Services().Update(ctx, svc)
}
