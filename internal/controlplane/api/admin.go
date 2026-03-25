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

// RaftStatusFn is a function that returns Raft cluster status for debugging.
type RaftStatusFn func() map[string]interface{}

// DNSToggler is the interface for starting/stopping the embedded DNS server at runtime.
type DNSToggler interface {
	Start() error
	Stop()
}

func (s *Server) registerAdminRoutes(r chi.Router) {
	r.Post("/admin/gc", s.triggerGC)
	r.Get("/admin/gc/status", s.gcStatus)
	r.Get("/admin/retention", s.retentionConfig)
	r.Post("/admin/dns", s.toggleDNS)
	r.Get("/debug/raft", s.getRaftStatus)
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

// SetRaftStatusFn sets the function that returns Raft cluster status.
func (s *Server) SetRaftStatusFn(fn RaftStatusFn) {
	s.raftStatusFn = fn
}

// SetDNSToggler sets the DNS server toggler for the admin endpoint.
func (s *Server) SetDNSToggler(dt DNSToggler) {
	s.dnsToggler = dt
}

func (s *Server) toggleDNS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if s.dnsToggler == nil {
		writeError(w, http.StatusServiceUnavailable, "DNS server not configured")
		return
	}

	if req.Enabled {
		if err := s.dnsToggler.Start(); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to start DNS server: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "dns enabled"})
	} else {
		s.dnsToggler.Stop()
		writeJSON(w, http.StatusOK, map[string]string{"status": "dns disabled"})
	}
}

func (s *Server) getRaftStatus(w http.ResponseWriter, r *http.Request) {
	if s.raftStatusFn == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not using raft coordinator"})
		return
	}
	writeJSON(w, http.StatusOK, s.raftStatusFn())
}
