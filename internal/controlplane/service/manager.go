package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/volume"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/pkg/slug"
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
	Mounts         []loka.Volume
	Autoscale      *loka.AutoscaleConfig
	DatabaseConfig *loka.DatabaseConfig // If set, deploy as a managed database instance.
	Uses           map[string]string   // Network ACL: alias→target service/db name.
	Replicas       int                 // Desired instance count (default 1). Creates N service records.
	RelationType   string              // "replica", "component", etc.
	ParentServiceID string             // Set on replicas/components to link to primary.
}

// DomainRouteRegistrar is an interface for registering/removing domain routes.
// This allows the service manager to update the domain proxy without importing
// the api package directly.
type DomainRouteRegistrar interface {
	AddRoute(route *loka.DomainRoute)
	RemoveRoute(domain string) bool
	ListRoutes() []*loka.DomainRoute
}

// Manager orchestrates service lifecycle.
type Manager struct {
	store         store.Store
	registry      *worker.Registry
	scheduler     *scheduler.Scheduler
	images        *image.Manager
	objStore      objstore.ObjectStore
	volumeManager *volume.Manager
	logger        *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	deploySem chan struct{} // semaphore limiting concurrent deploys

	logsFn func(serviceID string, lines int) ([]string, []string, error) // set by localworker
	proxy  DomainRouteRegistrar // domain proxy for route registration
}

// NewManager creates a new service manager.
func NewManager(s store.Store, reg *worker.Registry, sched *scheduler.Scheduler, imgMgr *image.Manager, objStore objstore.ObjectStore, volMgr *volume.Manager, logger *slog.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		store:         s,
		registry:      reg,
		scheduler:     sched,
		images:        imgMgr,
		objStore:      objStore,
		volumeManager: volMgr,
		logger:        logger,
		ctx:           ctx,
		cancel:        cancel,
		deploySem:     make(chan struct{}, 10), // max 10 concurrent deploys
	}
	m.wg.Add(1)
	go m.idleMonitor()

	// Recover services stuck in "deploying" from a previous crash.
	m.recoverStuckDeploys()
	m.recoverStuckDatabases()

	return m
}

// Close cancels all in-flight goroutines and waits for them to finish.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// recoverStuckDeploys marks services stuck in "deploying" as errors.
// This handles the case where the CP crashed during an async deploy.
func (m *Manager) recoverStuckDeploys() {
	ctx := context.Background()
	deploying := loka.ServiceStatusDeploying
	services, _, err := m.store.Services().List(ctx, store.ServiceFilter{Status: &deploying})
	if err != nil {
		m.logger.Warn("failed to check stuck deploys", "error", err)
		return
	}
	staleThreshold := time.Now().Add(-10 * time.Minute)
	for _, svc := range services {
		if svc.CreatedAt.Before(staleThreshold) {
			svc.Status = loka.ServiceStatusError
			svc.StatusMessage = "deploy interrupted by restart"
			svc.UpdatedAt = time.Now()
			if err := m.store.Services().Update(ctx, svc); err != nil {
				m.logger.Warn("failed to mark stuck service", "id", svc.ID, "error", err)
			} else {
				m.logger.Info("recovered stuck deploy", "id", svc.ID, "name", svc.Name)
			}
		}
	}
}

// recoverStuckDatabases handles database-specific stuck states after a crash.
func (m *Manager) recoverStuckDatabases() {
	ctx := context.Background()
	isDB := true
	dbs, _, err := m.store.Services().List(ctx, store.ServiceFilter{IsDatabase: &isDB, Limit: 500})
	if err != nil {
		m.logger.Warn("failed to check stuck databases", "error", err)
		return
	}
	now := time.Now()
	for _, db := range dbs {
		if db.DatabaseConfig == nil {
			continue
		}
		cfg := db.DatabaseConfig
		changed := false

		// Clean up expired credential grace periods.
		if cfg.PreviousLoginRole != "" && !cfg.GraceDeadline.IsZero() && now.After(cfg.GraceDeadline) {
			m.logger.Info("recovered expired credential grace period",
				"database", db.Name, "revoked", cfg.PreviousLoginRole)
			cfg.PreviousLoginRole = ""
			cfg.GraceDeadline = time.Time{}
			changed = true
		}

		// Clean up stuck upgrades (PreviousVersion set but service in error state).
		if cfg.PreviousVersion != "" && db.Status == loka.ServiceStatusError {
			m.logger.Info("recovered stuck upgrade",
				"database", db.Name, "previous_version", cfg.PreviousVersion)
			// Keep PreviousVersion so operator can rollback.
			db.StatusMessage = "upgrade interrupted by restart — use rollback to revert"
			changed = true
		}

		if changed {
			db.UpdatedAt = now
			m.store.Services().Update(ctx, db)
		}
	}
}

// Deploy creates a new service record and schedules it to a worker.
func (m *Manager) Deploy(ctx context.Context, opts DeployOpts) (*loka.Service, error) {
	if opts.Name == "" {
		opts.Name = slug.Generate()
	}
	// Check name uniqueness.
	existing, _, _ := m.store.Services().List(ctx, store.ServiceFilter{Name: &opts.Name, Limit: 1})
	if len(existing) > 0 {
		return nil, fmt.Errorf("service name %q already exists", opts.Name)
	}

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

	// Apply database engine defaults when deploying a managed database.
	if opts.DatabaseConfig != nil {
		defaults, err := loka.GetEngineDefaults(opts.DatabaseConfig.Engine, opts.DatabaseConfig.Version)
		if err != nil {
			return nil, err
		}
		if opts.ImageRef == "" {
			opts.ImageRef = defaults.Image
		}
		if opts.Port == 8080 { // override the generic default
			opts.Port = defaults.Port
		}
		// Merge database env vars.
		for k, v := range loka.DatabaseEnv(opts.DatabaseConfig) {
			if _, exists := opts.Env[k]; !exists {
				opts.Env[k] = v
			}
		}
		// Add database command args (e.g., redis --requirepass).
		if dbArgs := loka.DatabaseArgs(opts.DatabaseConfig); len(dbArgs) > 0 && len(opts.Args) == 0 {
			opts.Args = dbArgs
		}
		// Auto-create persistent volume for database data directory.
		volName := "db-" + opts.Name
		if m.volumeManager != nil {
			if _, err := m.volumeManager.Get(ctx, volName); err != nil {
				if _, err := m.volumeManager.Create(ctx, volName); err != nil {
					return nil, fmt.Errorf("create database volume %s: %w", volName, err)
				}
			}
		}
		opts.Mounts = append(opts.Mounts, loka.Volume{
			Name:     volName,
			Path:     defaults.DataDir,
			Provider: "volume",
			Access:   "readwrite",
		})
		// No domain routes for databases (internal only).
		opts.Routes = nil
		// Use TCP health check (empty path = port check).
		opts.HealthPath = ""
	}

	// Resolve `uses` dependencies: inject env vars for each target service/db.
	if len(opts.Uses) > 0 {
		for alias, targetName := range opts.Uses {
			targetSvc, _, _ := m.store.Services().List(ctx, store.ServiceFilter{Name: &targetName, Limit: 1})
			if len(targetSvc) == 0 {
				m.logger.Warn("dependency not found, skipping env injection", "alias", alias, "target", targetName)
				continue
			}
			target := targetSvc[0]
			dep := loka.ResolvedDependency{
				Alias:      alias,
				TargetName: target.Name,
				TargetID:   target.ID,
				Port:       target.Port,
				WorkerIP:   target.Name + ".loka.internal",
			}
			if target.ForwardPort > 0 {
				dep.WorkerIP = "127.0.0.1"
				dep.Port = target.ForwardPort
			}
			if target.DatabaseConfig != nil {
				dep.IsDatabase = true
				dep.Engine = target.DatabaseConfig.Engine
				dep.LoginRole = target.DatabaseConfig.LoginRole
				dep.Password = target.DatabaseConfig.Password
				dep.DBName = target.DatabaseConfig.DBName
			}
			for k, v := range loka.DependencyEnvVars(dep) {
				opts.Env[k] = v
			}
		}
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
		DatabaseConfig:  opts.DatabaseConfig,
		Uses:            opts.Uses,
		ParentServiceID: opts.ParentServiceID,
		Replicas:        opts.Replicas,
		RelationType:    opts.RelationType,
		LastActivity:    now,
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

	// Create additional replicas if requested.
	if opts.Replicas > 1 && opts.ParentServiceID == "" {
		for i := 1; i < opts.Replicas; i++ {
			replicaOpts := opts
			replicaOpts.Name = fmt.Sprintf("%s-replica-%d", svc.Name, i)
			replicaOpts.ParentServiceID = svc.ID
			replicaOpts.RelationType = "replica"
			replicaOpts.Replicas = 0 // Replicas don't spawn more replicas.
			replicaOpts.Routes = nil // Only primary gets domain routes.
			if _, err := m.Deploy(ctx, replicaOpts); err != nil {
				m.logger.Warn("failed to create replica", "primary", svc.Name, "replica", i, "error", err)
			}
		}
	}

	return svc, nil
}

// Scale adjusts the number of replicas for a service.
func (m *Manager) Scale(ctx context.Context, id string, replicas int) error {
	svc, err := m.store.Services().Get(ctx, id)
	if err != nil {
		return err
	}
	if replicas < 1 {
		return fmt.Errorf("replicas must be at least 1")
	}

	// Find current replicas.
	currentReplicas, _, _ := m.store.Services().List(ctx, store.ServiceFilter{
		PrimaryID: &svc.ID,
	})
	// Filter to only "replica" type.
	var replicaServices []*loka.Service
	for _, r := range currentReplicas {
		if r.RelationType == "replica" {
			replicaServices = append(replicaServices, r)
		}
	}
	currentCount := len(replicaServices) + 1 // +1 for primary

	if replicas > currentCount {
		// Scale up: create new replicas.
		for i := currentCount; i < replicas; i++ {
			replicaOpts := DeployOpts{
				Name:            fmt.Sprintf("%s-replica-%d", svc.Name, i),
				ImageRef:        svc.ImageRef,
				Command:         svc.Command,
				Args:            svc.Args,
				Env:             svc.Env,
				Workdir:         svc.Workdir,
				Port:            svc.Port,
				VCPUs:           svc.VCPUs,
				MemoryMB:        svc.MemoryMB,
				BundleKey:       svc.BundleKey,
				HealthPath:      svc.HealthPath,
				HealthInterval:  svc.HealthInterval,
				HealthTimeout:   svc.HealthTimeout,
				HealthRetries:   svc.HealthRetries,
				Labels:          svc.Labels,
				Mounts:          svc.Mounts,
				Uses:            svc.Uses,
				ParentServiceID: svc.ID,
				RelationType:    "replica",
			}
			if _, err := m.Deploy(ctx, replicaOpts); err != nil {
				m.logger.Warn("scale up: failed to create replica", "primary", svc.Name, "error", err)
			}
		}
	} else if replicas < currentCount {
		// Scale down: remove newest replicas.
		excess := currentCount - replicas
		for i := len(replicaServices) - 1; i >= 0 && excess > 0; i-- {
			if err := m.Destroy(ctx, replicaServices[i].ID); err != nil {
				m.logger.Warn("scale down: failed to remove replica", "id", replicaServices[i].ID, "error", err)
			}
			excess--
		}
	}

	// Update desired replica count on primary.
	svc.Replicas = replicas
	svc.UpdatedAt = time.Now()
	return m.store.Services().Update(ctx, svc)
}

// asyncDeploy handles image pulling, sending launch command, and health checking.
func (m *Manager) asyncDeploy(ctx context.Context, serviceID string, opts DeployOpts) {
	// Acquire deploy semaphore to limit concurrent deploys.
	select {
	case m.deploySem <- struct{}{}:
		defer func() { <-m.deploySem }()
	case <-ctx.Done():
		return
	}

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
			pullCtx, pullCancel := context.WithTimeout(ctx, 5*time.Minute)
			img, pullErr := m.images.Pull(pullCtx, svc.ImageRef)
			pullCancel()
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

	// Default workdir to /workspace when deploying a bundle.
	workdir := svc.Workdir
	if workdir == "" && svc.BundleKey != "" {
		workdir = "/workspace"
	}

	// Resolve warm snapshot paths if the image has them.
	// Downloads and decompresses from objstore on cache miss.
	var snapshotMemPath, snapshotVMStatePath string
	if svc.ImageID != "" && m.images != nil {
		memPath, statePath, snapErr := m.images.ResolveSnapshotPaths(ctx, svc.ImageID)
		if snapErr != nil {
			m.logger.Warn("resolve snapshot paths failed, will cold boot",
				"service", serviceID, "error", snapErr)
		} else {
			snapshotMemPath = memPath
			snapshotVMStatePath = statePath
		}
	}

	// Resolve layer-pack path if the image has one.
	var layerPackPath string
	if svc.ImageID != "" && m.images != nil {
		lp, lpErr := m.images.ResolveLayerPackPath(ctx, svc.ImageID)
		if lpErr != nil {
			m.logger.Debug("no layer-pack for image, using base rootfs",
				"service", serviceID, "error", lpErr)
		} else {
			layerPackPath = lp
		}
	}

	// If a bundle is provided, extract it into a named volume and mount
	// as read-only at /workspace instead of inline extraction via vsock.
	allMounts := svc.Mounts
	if svc.BundleKey != "" && m.volumeManager != nil {
		bundleVolName := fmt.Sprintf("bundle-%s-%s", svc.Name, svc.ID[:8])
		if err := m.volumeManager.ExtractBundle(ctx, bundleVolName, svc.BundleKey); err != nil {
			m.logger.Error("failed to extract bundle into volume — falling back to inline",
				"service", serviceID, "error", err)
		} else {
			bundleVol := loka.Volume{
				Path:     "/workspace",
				Provider: "bundle",
				Name:     bundleVolName,
				Access:   "readonly",
			}
			allMounts = append([]loka.Volume{bundleVol}, svc.Mounts...)
		}
	}

	launchData := worker.LaunchServiceData{
		ServiceID:           serviceID,
		ImageRef:            svc.ImageRef,
		VCPUs:               svc.VCPUs,
		MemoryMB:            svc.MemoryMB,
		RootfsPath:          rootfsPath,
		LayerPackPath:       layerPackPath,
		Command:             svc.Command,
		Args:                svc.Args,
		Env:                 svc.Env,
		Workdir:             workdir,
		Port:                svc.Port,
		BundleKey:           svc.BundleKey,
		Mounts:              allMounts,
		SnapshotMemPath:     snapshotMemPath,
		SnapshotVMStatePath: snapshotVMStatePath,
		HealthPath:          svc.HealthPath,
	}

	if err := m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "launch_service",
		Data: launchData,
	}); err != nil {
		m.setError(ctx, serviceID, fmt.Sprintf("failed to dispatch to worker: %v", err))
		return
	}

	// 3. Wait for health check to pass (polling with timeout).
	// Poll the store for status updates. The localworker updates the service
	// record when the process starts. We require multiple consecutive successful
	// status checks before marking the service as running.
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
		healthRetries = 3
	}

	// Total timeout for the entire health check phase.
	totalTimeout := healthInterval*time.Duration(healthRetries) + healthTimeout
	deadline := time.After(totalTimeout)
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()

	consecutiveOK := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			m.setError(ctx, serviceID, "health check timeout")
			return
		case <-ticker.C:
			// Poll service status by checking the service record in the store.
			s, err := m.store.Services().Get(ctx, serviceID)
			if err != nil {
				m.logger.Error("async deploy: health check poll failed", "service", serviceID, "error", err)
				consecutiveOK = 0
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

			// Count consecutive successful polls (service still in deploying state
			// means it hasn't errored out). After healthRetries consecutive OKs,
			// the launch command has had enough time to start the process.
			consecutiveOK++
			if consecutiveOK >= healthRetries {
				// Mark as running.
				s, err = m.store.Services().Get(ctx, serviceID)
				if err != nil {
					m.logger.Error("async deploy: failed to refresh service", "service", serviceID, "error", err)
					return
				}
				if s.Status != loka.ServiceStatusDeploying {
					return
				}
				s.Status = loka.ServiceStatusRunning
				s.Ready = true
				s.StatusMessage = ""
				s.LastActivity = time.Now()
				s.UpdatedAt = time.Now()
				if err := m.store.Services().Update(ctx, s); err != nil {
					m.logger.Error("async deploy: failed to mark service running", "service", serviceID, "error", err)
					return
				}
				// Register domain routes with the proxy.
				if m.proxy != nil && len(s.Routes) > 0 {
					for _, route := range s.Routes {
						if route.Domain == "" {
							continue
						}
						m.proxy.AddRoute(&loka.DomainRoute{
							Domain:     route.Domain,
							ServiceID:  s.ID,
							RemotePort: route.Port,
							Type:       loka.DomainRouteService,
						})
					}
				}
				m.logger.Info("service deployed and running", "service", serviceID)
				return
			}
		}
	}
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

	// Remove domain routes from the proxy.
	if m.proxy != nil {
		for _, route := range svc.Routes {
			if route.Domain != "" {
				m.proxy.RemoveRoute(route.Domain)
			}
		}
	}

	// Clean up bundle volume.
	bundleVolName := fmt.Sprintf("bundle-%s-%s", svc.Name, svc.ID[:8])
	if m.volumeManager != nil {
		volPath := m.volumeManager.BundlePath(bundleVolName)
		os.RemoveAll(volPath)
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

	// Remove domain routes from the proxy.
	if m.proxy != nil {
		for _, route := range svc.Routes {
			if route.Domain != "" {
				m.proxy.RemoveRoute(route.Domain)
			}
		}
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

	// Check worker affinity: if current worker is gone, reschedule.
	if svc.WorkerID != "" {
		if _, ok := m.registry.Get(svc.WorkerID); !ok {
			// Worker is down — pick a new one.
			wConn, err := m.scheduler.Pick(scheduler.Constraints{})
			if err == nil {
				m.logger.Info("redeploy: worker gone, rescheduling",
					"service", svc.Name, "old_worker", svc.WorkerID, "new_worker", wConn.Worker.ID)
				svc.WorkerID = wConn.Worker.ID
			}
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

	// Check if the assigned worker is still alive. If not, reschedule.
	if svc.WorkerID != "" {
		if _, ok := m.registry.Get(svc.WorkerID); !ok {
			wConn, err := m.scheduler.Pick(scheduler.Constraints{})
			if err != nil {
				return nil, fmt.Errorf("wake failover: no workers available")
			}
			m.logger.Info("wake failover: worker gone, rescheduling",
				"service", svc.Name, "old_worker", svc.WorkerID, "new_worker", wConn.Worker.ID)
			svc.WorkerID = wConn.Worker.ID
		}
	}

	svc.Status = loka.ServiceStatusWaking
	svc.StatusMessage = "waking"
	svc.UpdatedAt = time.Now()
	if err := m.store.Services().Update(ctx, svc); err != nil {
		return nil, err
	}

	// Determine snapshot paths: prefer app snapshot (instant, app already running)
	// over base image snapshot (requires bundle extract + service start).
	var snapshotMemPath, snapshotVMStatePath string
	isAppSnapshotRestore := false
	if svc.AppSnapshotMem != "" && svc.AppSnapshotState != "" {
		// Use app snapshot — instant restore with app already running.
		// Download from objstore if needed (same pattern as base snapshot).
		if m.objStore != nil {
			memPath, err := m.downloadSnapshot(svc.AppSnapshotMem)
			if err != nil {
				m.logger.Warn("failed to download app snapshot mem, falling back",
					"service", id, "error", err)
			} else {
				statePath, err := m.downloadSnapshot(svc.AppSnapshotState)
				if err != nil {
					m.logger.Warn("failed to download app snapshot state, falling back",
						"service", id, "error", err)
				} else {
					snapshotMemPath = memPath
					snapshotVMStatePath = statePath
					isAppSnapshotRestore = true
				}
			}
		}
	}
	if !isAppSnapshotRestore && svc.ImageID != "" && m.images != nil {
		// Fall back to base image snapshot.
		memPath, statePath, snapErr := m.images.ResolveSnapshotPaths(m.ctx, svc.ImageID)
		if snapErr != nil {
			m.logger.Warn("resolve snapshot paths failed for wake, will cold boot",
				"service", id, "error", snapErr)
		} else {
			snapshotMemPath = memPath
			snapshotVMStatePath = statePath
		}
	}

	// Default workdir to /workspace when deploying a bundle.
	workdir := svc.Workdir
	if workdir == "" && svc.BundleKey != "" {
		workdir = "/workspace"
	}

	// Resolve rootfs and layer-pack paths if we have an image.
	var rootfsPath, wakeLayerPackPath string
	if svc.ImageID != "" && m.images != nil {
		rp, err := m.images.ResolveRootfsPath(m.ctx, svc.ImageID)
		if err == nil {
			rootfsPath = rp
		}
		lp, err := m.images.ResolveLayerPackPath(m.ctx, svc.ImageID)
		if err == nil {
			wakeLayerPackPath = lp
		}
	}

	// Send resume command to worker.
	if svc.WorkerID != "" {
		m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "launch_service",
			Data: worker.LaunchServiceData{
				ServiceID:            svc.ID,
				ImageRef:             svc.ImageRef,
				VCPUs:                svc.VCPUs,
				MemoryMB:             svc.MemoryMB,
				RootfsPath:           rootfsPath,
				LayerPackPath:        wakeLayerPackPath,
				Command:              svc.Command,
				Args:                 svc.Args,
				Env:                  svc.Env,
				Workdir:              workdir,
				Port:                 svc.Port,
				BundleKey:            svc.BundleKey,
				Mounts:               svc.Mounts,
				SnapshotMemPath:      snapshotMemPath,
				SnapshotVMStatePath:  snapshotVMStatePath,
				IsAppSnapshotRestore: isAppSnapshotRestore,
				HealthPath:           svc.HealthPath,
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

// SetDomainProxy sets the domain proxy for automatic route registration.
// After setting the proxy, it registers routes for all currently running services.
func (m *Manager) SetDomainProxy(p DomainRouteRegistrar) {
	m.proxy = p
	m.registerExistingRoutes()
}

// registerExistingRoutes scans running services and registers their routes
// with the domain proxy. Called once when the proxy is first attached.
func (m *Manager) registerExistingRoutes() {
	if m.proxy == nil {
		return
	}
	running := loka.ServiceStatusRunning
	services, _, err := m.store.Services().List(m.ctx, store.ServiceFilter{Status: &running})
	if err != nil {
		m.logger.Error("failed to list running services for route registration", "error", err)
		return
	}
	for _, svc := range services {
		for _, route := range svc.Routes {
			if route.Domain == "" {
				continue
			}
			m.proxy.AddRoute(&loka.DomainRoute{
				Domain:     route.Domain,
				ServiceID:  svc.ID,
				RemotePort: route.Port,
				Type:       loka.DomainRouteService,
			})
		}
	}
	if len(services) > 0 {
		m.logger.Info("registered existing service routes", "services", len(services))
	}
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
			// Update store FIRST — only send stop/snapshot command if store update succeeds.
			svc.Status = loka.ServiceStatusIdle
			svc.Ready = false
			svc.UpdatedAt = time.Now()
			if err := m.store.Services().Update(m.ctx, svc); err != nil {
				m.logger.Error("idle monitor: failed to update service", "service_id", svc.ID, "error", err)
				continue // Don't send stop command if store update failed.
			}
			// Take an app snapshot before stopping so that wake is instant.
			// The snapshot_service handler will snapshot + stop the VM.
			if svc.WorkerID != "" {
				m.registry.SendCommand(svc.WorkerID, worker.WorkerCommand{
					Type: "snapshot_service",
					Data: worker.SnapshotServiceData{ServiceID: svc.ID},
				})
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

// downloadSnapshot downloads a snapshot file from the object store and returns
// the local path. The key format is "bucket/path" (e.g. "services/id/app_snapshot_mem").
func (m *Manager) downloadSnapshot(key string) (string, error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid snapshot key: %s", key)
	}
	bucket, objKey := parts[0], parts[1]

	reader, err := m.objStore.Get(m.ctx, bucket, objKey)
	if err != nil {
		return "", fmt.Errorf("download snapshot %s: %w", key, err)
	}
	defer reader.Close()

	tmpFile, err := os.CreateTemp("", "loka-snap-*")
	if err != nil {
		return "", fmt.Errorf("create temp for snapshot: %w", err)
	}

	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("write snapshot to temp: %w", err)
	}
	tmpFile.Close()
	return tmpFile.Name(), nil
}

// ListArtifacts returns files stored in the service's writable mounted volumes.
// This works even after the service is destroyed because volumes persist in objstore.
func (m *Manager) ListArtifacts(ctx context.Context, serviceID string) ([]loka.Artifact, error) {
	svc, err := m.store.Services().Get(ctx, serviceID)
	if err != nil {
		return nil, fmt.Errorf("service not found: %w", err)
	}

	if m.objStore == nil {
		return nil, fmt.Errorf("object store not configured")
	}

	var artifacts []loka.Artifact
	for _, mount := range svc.Mounts {
		if mount.Access != "readonly" && mount.Provider == "volume" && mount.Name != "" {
			objects, err := m.objStore.List(ctx, "volumes", mount.Name+"/")
			if err != nil {
				m.logger.Warn("failed to list volume files", "volume", mount.Name, "error", err)
				continue
			}
			for _, obj := range objects {
				path := strings.TrimPrefix(obj.Key, mount.Name+"/")
				if path == "" {
					continue
				}
				hash := fmt.Sprintf("%x", obj.Key)
				if len(hash) > 16 {
					hash = hash[:16]
				}
				artifacts = append(artifacts, loka.Artifact{
					ID:        hash,
					SessionID: serviceID, // Use service ID in the session field.
					Path:      mount.Path + "/" + path,
					Size:      obj.Size,
					Type:      "added",
					CreatedAt: obj.LastModified,
				})
			}
		}
	}
	return artifacts, nil
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
