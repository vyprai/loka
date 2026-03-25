package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vyprai/loka/internal/config"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/provider"
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
	domainProxy      *DomainProxy // Domain proxy for subdomain routing.
	raftStatusFn     RaftStatusFn // Optional: returns Raft cluster status for debug endpoint.
	dnsToggler       DNSToggler   // Optional: toggles the embedded DNS server at runtime.
}

// ServerOpts holds optional configuration for the API server.
type ServerOpts struct {
	APIKey         string                  // If set, require this key for API access.
	GC             GCRunner                // Garbage collector (optional).
	CACertPath     string                  // Path to CA certificate for /ca.crt endpoint.
	Retention      config.RetentionConfig  // Retention configuration.
	ObjStore       objstore.ObjectStore    // Object store (exposed to workers/HA nodes).
	ServiceManager *service.Manager        // Service manager (optional).
	DomainProxy    *DomainProxy            // Domain proxy for subdomain-based routing (optional).
}

// NewServer creates a new API server.
func NewServer(sm *session.Manager, reg *worker.Registry, provReg *provider.Registry, imgMgr *image.Manager, drainer *worker.Drainer, s store.Store, logger *slog.Logger, opts ...ServerOpts) *Server {
	var o ServerOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	srv := &Server{
		router:           chi.NewRouter(),
		sessionManager:   sm,
		serviceManager:   o.ServiceManager,
		workerRegistry:   reg,
		providerRegistry: provReg,
		imageManager:     imgMgr,
		drainer:          drainer,
		store:            s,
		objStore:         o.ObjStore,
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

// serveCACert serves the auto-generated CA certificate.
// Clients use this to bootstrap TLS trust:
//   curl -k https://server:6840/ca.crt -o ca.crt
//   loka connect https://server:6840 --ca-cert ca.crt
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
		r.Get("/services/{id}/logs", s.getServiceLogs)
		r.Post("/services/{id}/routes", s.addServiceRoute)
		r.Delete("/services/{id}/routes/{subdomain}", s.removeServiceRoute)
		r.Get("/services/{id}/routes", s.listServiceRoutes)

		// Object store (service bundles) — public API for CLI uploads.
		r.Put("/objstore/objects/{bucket}/*", s.objStorePut)
		r.Get("/objstore/objects/{bucket}/*", s.objStoreGet)

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

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
