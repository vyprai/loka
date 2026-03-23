package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/provider"
	"github.com/vyprai/loka/internal/store"
)

// Server is the control plane HTTP API server.
type Server struct {
	router           *chi.Mux
	sessionManager   *session.Manager
	workerRegistry   *worker.Registry
	providerRegistry *provider.Registry
	imageManager     *image.Manager
	drainer          *worker.Drainer
	store            store.Store
	logger           *slog.Logger
	apiKey           string
}

// ServerOpts holds optional configuration for the API server.
type ServerOpts struct {
	APIKey string // If set, require this key for API access.
}

// NewServer creates a new API server.
func NewServer(sm *session.Manager, reg *worker.Registry, provReg *provider.Registry, imgMgr *image.Manager, drainer *worker.Drainer, s store.Store, logger *slog.Logger, opts ...ServerOpts) *Server {
	var apiKey string
	if len(opts) > 0 {
		apiKey = opts[0].APIKey
	}
	srv := &Server{
		router:           chi.NewRouter(),
		sessionManager:   sm,
		workerRegistry:   reg,
		providerRegistry: provReg,
		imageManager:     imgMgr,
		drainer:          drainer,
		store:            s,
		logger:           logger,
		apiKey:           apiKey,
	}
	srv.routes()
	srv.registerInternalRoutes()
	return srv
}

// Handler returns the http.Handler for this server.
func (s *Server) Handler() http.Handler {
	return s.router
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

	r.Route("/api/v1", func(r chi.Router) {
		// Sessions
		r.Post("/sessions", s.createSession)
		r.Get("/sessions", s.listSessions)
		r.Get("/sessions/{id}", s.getSession)
		r.Delete("/sessions/{id}", s.destroySession)
		r.Post("/sessions/{id}/pause", s.pauseSession)
		r.Post("/sessions/{id}/resume", s.resumeSession)
		r.Post("/sessions/{id}/mode", s.setSessionMode)
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

		// Sessions - migration
		r.Post("/sessions/{id}/migrate", s.migrateSession)

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

		// Health
		r.Get("/health", s.health)
	})
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
