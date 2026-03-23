package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type execReq struct {
	Commands []commandReq `json:"commands"`
	Parallel bool         `json:"parallel"`
	// Shorthand for single command.
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Workdir string            `json:"workdir"`
	Env     map[string]string `json:"env"`
}

type commandReq struct {
	ID      string            `json:"id"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Workdir string            `json:"workdir"`
	Env     map[string]string `json:"env"`
}

func (s *Server) execCommand(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	var req execReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var commands []loka.Command

	// Handle single command shorthand.
	if req.Command != "" {
		commands = []loka.Command{{
			ID:      uuid.New().String(),
			Command: req.Command,
			Args:    req.Args,
			Workdir: req.Workdir,
			Env:     req.Env,
		}}
	} else {
		for _, c := range req.Commands {
			id := c.ID
			if id == "" {
				id = uuid.New().String()
			}
			commands = append(commands, loka.Command{
				ID:      id,
				Command: c.Command,
				Args:    c.Args,
				Workdir: c.Workdir,
				Env:     c.Env,
			})
		}
	}

	if len(commands) == 0 {
		writeError(w, http.StatusBadRequest, "no commands provided")
		return
	}

	exec, err := s.sessionManager.Exec(r.Context(), sessionID, commands, req.Parallel)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, exec)
}

func (s *Server) getExecution(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "execId")
	exec, err := s.sessionManager.GetExecution(r.Context(), execID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

// approveExecution approves a pending command.
//
// Body:
//
//	{
//	  "scope": "once"       — approve this one execution only (default)
//	  "scope": "command"    — approve this command binary for the rest of the session
//	  "scope": "always"     — add to permanent whitelist (persisted in session policy)
//	}
func (s *Server) approveExecution(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	execID := chi.URLParam(r, "execId")
	var req struct {
		Scope string `json:"scope"` // "once", "command", "always"
	}
	decodeJSON(r, &req)
	if req.Scope == "" {
		req.Scope = "once"
	}

	addToWhitelist := req.Scope == "command" || req.Scope == "always"

	exec, err := s.sessionManager.ApproveExecution(r.Context(), sessionID, execID, addToWhitelist)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

func (s *Server) rejectExecution(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	execID := chi.URLParam(r, "execId")
	var req struct {
		Reason string `json:"reason"`
	}
	decodeJSON(r, &req)
	if req.Reason == "" {
		req.Reason = "rejected by operator"
	}
	exec, err := s.sessionManager.RejectExecution(r.Context(), sessionID, execID, req.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

func (s *Server) cancelExecution(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	execID := chi.URLParam(r, "execId")
	exec, err := s.sessionManager.CancelExecution(r.Context(), sessionID, execID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

func (s *Server) listExecutions(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "id")
	var filter store.ExecutionFilter
	if status := r.URL.Query().Get("status"); status != "" {
		st := loka.ExecStatus(status)
		filter.Status = &st
	}
	execs, err := s.sessionManager.ListExecutions(r.Context(), sessionID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if execs == nil {
		execs = []*loka.Execution{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"executions": execs,
		"total":      len(execs),
	})
}
