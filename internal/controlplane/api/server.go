package api

import (
	"encoding/json"
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vyprai/loka/internal/config"
	"github.com/vyprai/loka/internal/controlplane/database"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/lock"
	"github.com/vyprai/loka/internal/controlplane/task"
	"github.com/vyprai/loka/internal/controlplane/volume"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/provider"
	"github.com/vyprai/loka/internal/registry"
	"github.com/vyprai/loka/internal/store"
)

// Server is the control plane HTTP API server.
type Server struct {
	router           *chi.Mux
	sessionManager   *session.Manager
	serviceManager   *service.Manager
	workerRegistry   *worker.Registry
	providerRegistry *provider.Registry
	imageManager     *image.Manager
	drainer          *worker.Drainer
	store            store.Store
	objStore         objstore.ObjectStore
	logger           *slog.Logger
	apiKey           string
	gc               GCRunner
	retention        config.RetentionConfig
	caCertPath       string // Path to CA certificate (served at /ca.crt).
	volumeManager    *volume.Manager // Named volume lifecycle manager.
	lockManager      *lock.Manager  // Distributed file lock manager.
	taskManager      *task.Manager  // One-time task manager.
	backupManager    *database.BackupManager // Database backup scheduler.
	domainProxy      *DomainProxy // Domain proxy for domain routing.
	registryStore    *registry.Store // OCI registry blob/manifest store.
	adminKey         string          // Separate key for admin endpoints (optional; if empty, same as apiKey).
	raftStatusFn     RaftStatusFn    // Optional: returns Raft cluster status for debug endpoint.
	dnsToggler       DNSToggler      // Optional: toggles the embedded DNS server at runtime.
}

// ServerOpts holds optional configuration for the API server.
type ServerOpts struct {
	APIKey         string                  // If set, require this key for API access.
	GC             GCRunner                // Garbage collector (optional).
	CACertPath     string                  // Path to CA certificate for /ca.crt endpoint.
	Retention      config.RetentionConfig  // Retention configuration.
	ObjStore       objstore.ObjectStore    // Object store (exposed to workers/HA nodes).
	DataDir        string                  // Data directory for volume storage.
	ServiceManager *service.Manager        // Service manager (optional).
	TaskManager    *task.Manager           // Task manager (optional).
	DomainProxy    *DomainProxy            // Domain proxy for domain-based routing (optional).
}

// NewServer creates a new API server.
func NewServer(sm *session.Manager, reg *worker.Registry, provReg *provider.Registry, imgMgr *image.Manager, drainer *worker.Drainer, s store.Store, logger *slog.Logger, opts ...ServerOpts) *Server {
	var o ServerOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	// Initialize OCI registry store if object store is available.
	var regStore *registry.Store
	if o.ObjStore != nil {
		regStore = registry.NewStore(o.ObjStore)
	}

	// Create volume manager if object store is available.
	var volMgr *volume.Manager
	if o.ObjStore != nil {
		volMgr = volume.NewManager(s, o.ObjStore, o.DataDir, logger)
	}

	// Create backup manager if object store is available.
	var backupMgr *database.BackupManager
	if o.ObjStore != nil {
		backupMgr = database.NewBackupManager(s, o.ObjStore, logger)
	}

	srv := &Server{
		router:           chi.NewRouter(),
		sessionManager:   sm,
		serviceManager:   o.ServiceManager,
		volumeManager:    volMgr,
		backupManager:    backupMgr,
		lockManager:      lock.NewManager(extractDB(s)),
		taskManager:      o.TaskManager,
		workerRegistry:   reg,
		providerRegistry: provReg,
		imageManager:     imgMgr,
		drainer:          drainer,
		store:            s,
		objStore:         o.ObjStore,
		registryStore:    regStore,
		logger:           logger,
		apiKey:           o.APIKey,
		gc:               o.GC,
		retention:        o.Retention,
		caCertPath:       o.CACertPath,
		domainProxy:      o.DomainProxy,
	}
	srv.routes()
	srv.registerInternalRoutes()
	return srv
}

// Handler returns the http.Handler for this server.
func (s *Server) Handler() http.Handler {
	return s.router
}

// RegistryStore returns the OCI registry store, or nil if not configured.
func (s *Server) RegistryStore() *registry.Store {
	return s.registryStore
}

// VolumeManager returns the named volume manager, or nil if not configured.
func (s *Server) VolumeManager() *volume.Manager {
	return s.volumeManager
}

// NewRegistryAPI creates an OCI Distribution Spec registry server backed by
// this server's registry store. Returns nil if no registry store is configured.
func (s *Server) NewRegistryAPI() *RegistryServer {
	if s.registryStore == nil {
		return nil
	}
	return NewRegistryServer(s.registryStore, s.logger, s.apiKey)
}

// serveCACert serves the auto-generated CA certificate.
// Clients use this to bootstrap TLS trust:
//   curl -k https://server:6840/ca.crt -o ca.crt
//   loka space connect https://server:6840 --ca-cert ca.crt
func (s *Server) serveCACert(w http.ResponseWriter, r *http.Request) {
	if s.caCertPath == "" {
		writeError(w, http.StatusNotFound, "no CA certificate configured")
		return
	}
	data, err := os.ReadFile(s.caCertPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read CA certificate")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", "attachment; filename=ca.crt")
	w.Write(data)
}

func (s *Server) routes() {
	r := s.router

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(metricsMiddleware)
	r.Use(apiKeyAuth(s.apiKey))

	// Prometheus metrics endpoint (no auth).
	r.Handle("/metrics", promhttp.Handler())

	// CA cert endpoint (no auth) — intentionally unauthenticated.
	// The CA certificate is public information (not a secret) and must be
	// accessible without auth so that new clients can bootstrap TLS trust:
	//   curl -k https://server:6840/ca.crt -o ca.crt
	// This is analogous to how ACME/Let's Encrypt serves CA certs publicly.
	r.Get("/ca.crt", s.serveCACert)

	r.Route("/api/v1", func(r chi.Router) {
		// Sessions
		r.Post("/sessions", s.createSession)
		r.Get("/sessions", s.listSessions)
		r.Get("/sessions/{id}", s.getSession)
		r.Delete("/sessions/{id}", s.destroySession)
		r.Post("/sessions/{id}/pause", s.pauseSession)
		r.Post("/sessions/{id}/resume", s.resumeSession)
		r.Post("/sessions/{id}/mode", s.setSessionMode)
		r.Post("/sessions/{id}/idle", s.idleSession)
		r.Get("/sessions/{id}/whitelist", s.getWhitelist)
		r.Put("/sessions/{id}/whitelist", s.updateWhitelist)

		// Executions
		r.Post("/sessions/{id}/exec", s.execCommand)
		r.Get("/sessions/{id}/exec", s.listExecutions)
		r.Get("/sessions/{id}/exec/{execId}", s.getExecution)
		r.Post("/sessions/{id}/exec/{execId}/approve", s.approveExecution)
		r.Post("/sessions/{id}/exec/{execId}/reject", s.rejectExecution)
		r.Delete("/sessions/{id}/exec/{execId}", s.cancelExecution)
		r.Get("/sessions/{id}/exec/{execId}/stream", s.streamExecution)
		r.Post("/sessions/{id}/exec/stream", s.execAndStream)

		// Checkpoints
		r.Post("/sessions/{id}/checkpoints", s.createCheckpoint)
		r.Get("/sessions/{id}/checkpoints", s.listCheckpoints)
		r.Post("/sessions/{id}/checkpoints/{cpId}/restore", s.restoreCheckpoint)
		r.Delete("/sessions/{id}/checkpoints/{cpId}", s.deleteCheckpoint)
		r.Get("/sessions/{id}/checkpoints/diff", s.diffCheckpoints)

		// Artifacts
		r.Get("/sessions/{id}/artifacts", s.listArtifacts)
		r.Get("/sessions/{id}/artifacts/download", s.downloadArtifact)
		r.Get("/sessions/{id}/checkpoints/{cpId}/artifacts", s.listCheckpointArtifacts)

		// Storage sync
		r.Post("/sessions/{id}/sync", s.syncMount)


		// Sessions - migration
		r.Post("/sessions/{id}/migrate", s.migrateSession)

		// Services
		r.Post("/services", s.deployService)
		r.Get("/services", s.listServices)
		r.Get("/services/{id}", s.getService)
		r.Delete("/services/{id}", s.destroyService)
		r.Post("/services/{id}/stop", s.stopService)
		r.Post("/services/{id}/redeploy", s.redeployService)
		r.Put("/services/{id}/env", s.updateServiceEnv)
		r.Post("/services/{id}/scale", s.scaleService)
		r.Get("/services/{id}/logs", s.getServiceLogs)
		r.Post("/services/{id}/routes", s.addServiceRoute)
		r.Delete("/services/{id}/routes/{domain}", s.removeServiceRoute)
		r.Get("/services/{id}/routes", s.listServiceRoutes)

		// Databases — managed database instances.
		r.Post("/databases", s.createDatabase)
		r.Get("/databases", s.listDatabases)
		r.Get("/databases/{id}", s.getDatabase)
		r.Delete("/databases/{id}", s.destroyDatabase)
		r.Post("/databases/{id}/stop", s.stopDatabase)
		r.Post("/databases/{id}/start", s.startDatabase)
		r.Get("/databases/{id}/logs", s.getDatabaseLogs)
		r.Get("/databases/{id}/credentials", s.getDatabaseCredentials)
		r.Post("/databases/{id}/credentials/rotate", s.rotateDatabaseCredentials)
		r.Put("/databases/{id}/credentials", s.setDatabaseCredentials)
		r.Post("/databases/{id}/replicas", s.addDatabaseReplica)
		r.Delete("/databases/{id}/replicas/{rid}", s.removeDatabaseReplica)
		r.Get("/databases/{id}/replicas", s.listDatabaseReplicas)
		r.Post("/databases/{id}/backups", s.createDatabaseBackup)
		r.Get("/databases/{id}/backups", s.listDatabaseBackups)
		r.Post("/databases/{id}/restore", s.restoreDatabase)
		r.Post("/databases/{id}/backups/{backupId}/verify", s.verifyDatabaseBackup)
		r.Post("/databases/{id}/upgrade", s.upgradeDatabase)
		r.Post("/databases/{id}/upgrade/rollback", s.rollbackDatabaseUpgrade)
		r.Post("/databases/{id}/force-stop", s.forceStopDatabase)

		// Object store — public API.
		r.Put("/objstore/objects/{bucket}/*", s.objStorePut)
		r.Get("/objstore/objects/{bucket}/*", s.objStoreGet)
		r.Head("/objstore/objects/{bucket}/*", s.objStoreHead)
		r.Delete("/objstore/objects/{bucket}/*", s.objStoreDelete)
		r.Get("/objstore/list/{bucket}", s.objStoreList)

		// Workers
		r.Get("/workers", s.listWorkers)
		r.Get("/workers/{id}", s.getWorker)
		r.Post("/workers/{id}/drain", s.drainWorker)
		r.Post("/workers/{id}/undrain", s.undrainWorker)
		r.Delete("/workers/{id}", s.removeWorker)
		r.Put("/workers/{id}/labels", s.labelWorker)

		// Images (Docker-based base images)
		r.Post("/images/pull", s.pullImage)
		r.Get("/images", s.listImages)
		r.Get("/images/{id}", s.getImage)
		r.Delete("/images/{id}", s.deleteImage)

		// Providers
		r.Get("/providers", s.listProviders)
		r.Post("/providers/{provider}/provision", s.provisionWorkers)
		r.Delete("/providers/{provider}/workers/{workerId}", s.deprovisionWorker)
		r.Get("/providers/{provider}/status", s.providerStatus)

		// Worker Tokens
		r.Post("/worker-tokens", s.createWorkerToken)
		r.Get("/worker-tokens", s.listWorkerTokens)
		r.Delete("/worker-tokens/{tokenId}", s.revokeWorkerToken)

		// Volumes
		r.Post("/volumes", s.createVolume)
		r.Get("/volumes", s.listVolumes)
		r.Get("/volumes/{name}", s.getVolume)
		r.Delete("/volumes/{name}", s.deleteVolume)

		// Volume locks
		r.Post("/volumes/{name}/lock", s.acquireVolumeLock)
		r.Delete("/volumes/{name}/lock", s.releaseVolumeLock)
		r.Get("/volumes/{name}/locks", s.listVolumeLocks)

		// Tasks
		s.registerTaskRoutes(r)

		// Image Registry Management
		s.registerRegistryRoutes(r)

		// Admin
		s.registerAdminRoutes(r)

		// Health
		r.Get("/health", s.health)
	})

	// Register domain proxy routes (expose/unexpose/list) if proxy is available.
	if s.domainProxy != nil {
		s.registerDomainRoutes(s.router, s.domainProxy)
	}
}

// JSON helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Error code constants for standardized API error responses.
const (
	ErrCodeBadRequest     = "BAD_REQUEST"
	ErrCodeUnauthorized   = "UNAUTHORIZED"
	ErrCodeForbidden      = "FORBIDDEN"
	ErrCodeNotFound       = "NOT_FOUND"
	ErrCodeConflict       = "CONFLICT"
	ErrCodeTooMany        = "TOO_MANY_REQUESTS"
	ErrCodeInternal       = "INTERNAL_ERROR"
	ErrCodeUnavailable    = "SERVICE_UNAVAILABLE"
	ErrCodeDBNotFound     = "DATABASE_NOT_FOUND"
	ErrCodeInvalidEngine  = "INVALID_ENGINE"
	ErrCodeInvalidVersion = "INVALID_VERSION"
	ErrCodeBackupNotFound = "BACKUP_NOT_FOUND"
	ErrCodeUpgradeFailed  = "UPGRADE_FAILED"
)

// statusToCode maps HTTP status codes to default error codes.
func statusToCode(status int) string {
	switch status {
	case 400:
		return ErrCodeBadRequest
	case 401:
		return ErrCodeUnauthorized
	case 403:
		return ErrCodeForbidden
	case 404:
		return ErrCodeNotFound
	case 409:
		return ErrCodeConflict
	case 429:
		return ErrCodeTooMany
	case 503:
		return ErrCodeUnavailable
	default:
		return ErrCodeInternal
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    statusToCode(status),
			"message": msg,
			"status":  status,
		},
	})
}

// writeErrorCode writes an error with an explicit error code.
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": msg,
			"status":  status,
		},
	})
}

func decodeJSON(r *http.Request, v any) error {
	// Limit request body to 10MB to prevent OOM attacks.
	r.Body = http.MaxBytesReader(nil, r.Body, 10*1024*1024)
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// dbProvider is implemented by store backends that expose their *sql.DB.
type dbProvider interface {
	DB() *sql.DB
}

// extractDB returns the underlying *sql.DB from a store, or nil if unavailable.
func extractDB(s store.Store) *sql.DB {
	if p, ok := s.(dbProvider); ok {
		return p.DB()
	}
	return nil
}
