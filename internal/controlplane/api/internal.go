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
