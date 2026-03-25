package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	var filter store.WorkerFilter
	if provider := r.URL.Query().Get("provider"); provider != "" {
		filter.Provider = &provider
	}
	if status := r.URL.Query().Get("status"); status != "" {
		st := loka.WorkerStatus(status)
		filter.Status = &st
	}

	workers, err := s.store.Workers().List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if workers == nil {
		workers = []*loka.Worker{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workers": workers,
		"total":   len(workers),
	})
}

func (s *Server) getWorker(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	worker, err := s.store.Workers().Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

type drainWorkerReq struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

func (s *Server) drainWorker(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req drainWorkerReq
	if err := decodeJSON(r, &req); err != nil {
		req.TimeoutSeconds = 300 // Default 5 minutes.
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 300
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if err := s.drainer.Drain(r.Context(), id, timeout); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	worker, _ := s.store.Workers().Get(r.Context(), id)
	writeJSON(w, http.StatusOK, worker)
}

func (s *Server) undrainWorker(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.drainer.Undrain(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	worker, _ := s.store.Workers().Get(r.Context(), id)
	writeJSON(w, http.StatusOK, worker)
}

func (s *Server) removeWorker(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))

	wkr, err := s.store.Workers().Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if !force {
		// Check if worker has running sessions.
		sessions, _ := s.store.Sessions().ListByWorker(r.Context(), id)
		active := 0
		for _, sess := range sessions {
			if sess.Status != loka.SessionStatusTerminated {
				active++
			}
		}
		if active > 0 {
			writeError(w, http.StatusConflict, "worker has active sessions; drain first or use force=true")
			return
		}
	}

	// Unregister from registry.
	s.workerRegistry.Unregister(id)

	// Delete from store.
	s.store.Workers().Delete(r.Context(), id)
	_ = wkr

	w.WriteHeader(http.StatusNoContent)
}

type labelWorkerReq struct {
	Labels map[string]string `json:"labels"`
}

func (s *Server) labelWorker(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req labelWorkerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	wkr, err := s.store.Workers().Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if wkr.Labels == nil {
		wkr.Labels = make(map[string]string)
	}
	for k, v := range req.Labels {
		if v == "" {
			delete(wkr.Labels, k) // Empty value = remove label.
		} else {
			wkr.Labels[k] = v
		}
	}
	wkr.UpdatedAt = time.Now()
	if err := s.store.Workers().Update(r.Context(), wkr); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update registry.
	if conn, ok := s.workerRegistry.Get(id); ok {
		conn.Worker.Labels = wkr.Labels
	}

	writeJSON(w, http.StatusOK, wkr)
}

func (s *Server) migrateSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		TargetWorkerID string `json:"target_worker_id"`
	}
	if err := decodeJSON(r, &req); err != nil || req.TargetWorkerID == "" {
		writeError(w, http.StatusBadRequest, "target_worker_id required")
		return
	}

	if err := s.sessionManager.MigrateSession(r.Context(), sessionID, req.TargetWorkerID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess, _ := s.sessionManager.Get(r.Context(), sessionID)
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	workers := s.workerRegistry.List()
	ready := 0
	for _, w := range workers {
		if w.Worker.Status == loka.WorkerStatusReady || w.Worker.Status == loka.WorkerStatusBusy {
			ready++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"workers_total": len(workers),
		"workers_ready": ready,
	})
}
