package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/loka"
)

// registerInternalRoutes adds internal API routes used by workers.
// All internal routes require a valid worker token via Bearer authentication.
func (s *Server) registerInternalRoutes() {
	s.router.Route("/api/internal", func(r chi.Router) {
		r.Use(workerTokenAuth(s.store))
		r.Post("/workers/register", s.internalRegisterWorker)
		r.Post("/workers/heartbeat", s.internalWorkerHeartbeat)
		r.Post("/exec/complete", s.internalExecComplete)
		r.Post("/sessions/status", s.internalSessionStatus)

		// Object store proxy — workers and HA non-leaders read/write through the leader.
		r.Put("/objstore/objects/{bucket}/*", s.objStorePut)
		r.Get("/objstore/objects/{bucket}/*", s.objStoreGet)
		r.Head("/objstore/objects/{bucket}/*", s.objStoreHead)
		r.Delete("/objstore/objects/{bucket}/*", s.objStoreDelete)
		r.Get("/objstore/list/{bucket}", s.objStoreList)
	})
}

type registerWorkerReq struct {
	Hostname string                `json:"hostname"`
	Provider string                `json:"provider"`
	Capacity loka.ResourceCapacity `json:"capacity"`
	Labels   map[string]string     `json:"labels"`
}

func (s *Server) internalRegisterWorker(w http.ResponseWriter, r *http.Request) {
	var req registerWorkerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	worker, err := s.workerRegistry.Register(r.Context(),
		req.Hostname, "", req.Provider, "", "", "dev",
		req.Capacity, req.Labels, false,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"worker_id": worker.ID})
}

type heartbeatReq struct {
	WorkerID     string             `json:"worker_id"`
	Status       string             `json:"status"`
	SessionCount int                `json:"session_count"`
	SessionIDs   []string           `json:"session_ids"`
	Usage        loka.ResourceUsage `json:"usage"`
}

func (s *Server) internalWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.WorkerID == "" {
		writeError(w, http.StatusBadRequest, "worker_id required")
		return
	}
	// Validate heartbeat data to prevent falsified accounting.
	if req.SessionCount < 0 {
		writeError(w, http.StatusBadRequest, "session_count must be non-negative")
		return
	}
	if req.Usage.CPUPercent < 0 || req.Usage.CPUPercent > 100 {
		writeError(w, http.StatusBadRequest, "CPU usage percentage must be 0-100")
		return
	}
	if req.Usage.MemoryUsedMB < 0 || req.Usage.DiskUsedMB < 0 {
		writeError(w, http.StatusBadRequest, "memory/disk usage must be non-negative")
		return
	}

	hb := &loka.Heartbeat{
		WorkerID:     req.WorkerID,
		Timestamp:    time.Now(),
		Status:       loka.WorkerStatus(req.Status),
		SessionCount: req.SessionCount,
		SessionIDs:   req.SessionIDs,
		Usage:        req.Usage,
	}
	if err := s.workerRegistry.UpdateHeartbeat(r.Context(), req.WorkerID, hb); err != nil {
		// Worker not found — tell it to re-register.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"status": "unknown_worker",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type execCompleteReq struct {
	SessionID string               `json:"session_id"`
	ExecID    string               `json:"exec_id"`
	Status    string               `json:"status"`
	Results   []loka.CommandResult `json:"results"`
	Error     string               `json:"error"`
}

func (s *Server) internalExecComplete(w http.ResponseWriter, r *http.Request) {
	var req execCompleteReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if err := s.sessionManager.CompleteExecution(r.Context(), req.ExecID,
		loka.ExecStatus(req.Status), req.Results, req.Error); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

type sessionStatusReq struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func (s *Server) internalSessionStatus(w http.ResponseWriter, r *http.Request) {
	var req sessionStatusReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	sess, err := s.store.Sessions().Get(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	sess.Status = loka.SessionStatus(req.Status)
	sess.UpdatedAt = time.Now()
	if err := s.store.Sessions().Update(r.Context(), sess); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
