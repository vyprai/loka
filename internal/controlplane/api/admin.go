package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/config"
)

// GCRunner is the interface for the garbage collector used by admin endpoints.
type GCRunner interface {
	Sweep(ctx context.Context)
	LastResult() any
}

func (s *Server) registerAdminRoutes(r chi.Router) {
	r.Post("/admin/gc", s.triggerGC)
	r.Get("/admin/gc/status", s.gcStatus)
	r.Get("/admin/retention", s.retentionConfig)
}

func (s *Server) triggerGC(w http.ResponseWriter, r *http.Request) {
	if s.gc == nil {
		writeError(w, http.StatusServiceUnavailable, "GC not configured")
		return
	}
	go s.gc.Sweep(context.Background())
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sweep started"})
}

func (s *Server) gcStatus(w http.ResponseWriter, r *http.Request) {
	if s.gc == nil {
		writeError(w, http.StatusServiceUnavailable, "GC not configured")
		return
	}
	result := s.gc.LastResult()
	if result == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no sweep has run yet"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) retentionConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.retention)
}

// SetGC sets the garbage collector on the server after construction.
func (s *Server) SetGC(gc GCRunner) {
	s.gc = gc
}

// SetRetention sets the retention config on the server.
func (s *Server) SetRetention(rc config.RetentionConfig) {
	s.retention = rc
}
