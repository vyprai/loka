package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type createSessionReq struct {
	Name            string                `json:"name"`
	Image           string                `json:"image"`       // Docker image reference: "ubuntu:22.04"
	SnapshotID      string                `json:"snapshot_id"` // Optional: restore from snapshot.
	Mode            string                `json:"mode"`
	VCPUs           int                   `json:"vcpus"`
	MemoryMB        int                   `json:"memory_mb"`
	Labels          map[string]string     `json:"labels"`
	AllowedCommands []string              `json:"allowed_commands,omitempty"`
	BlockedCommands []string              `json:"blocked_commands,omitempty"`
	NetworkPolicy   *loka.NetworkPolicy   `json:"network_policy,omitempty"`
	ExecPolicy      *loka.ExecPolicy      `json:"exec_policy,omitempty"`
	Mounts          []loka.StorageMount   `json:"mounts,omitempty"`
	Ports           []loka.PortMapping    `json:"ports,omitempty"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	mode := loka.ExecMode(req.Mode)
	if mode == "" {
		mode = loka.ModeExplore
	}

	// Build exec policy from request.
	var execPolicy *loka.ExecPolicy
	if req.ExecPolicy != nil {
		execPolicy = req.ExecPolicy
	} else if len(req.AllowedCommands) > 0 || len(req.BlockedCommands) > 0 || req.NetworkPolicy != nil {
		p := loka.DefaultExecPolicy()
		p.AllowedCommands = req.AllowedCommands
		p.BlockedCommands = req.BlockedCommands
		p.NetworkPolicy = req.NetworkPolicy
		execPolicy = &p
	}

	sess, err := s.sessionManager.Create(r.Context(), session.CreateOpts{
		Name:       req.Name,
		ImageRef:   req.Image,
		SnapshotID: req.SnapshotID,
		Mode:       mode,
		VCPUs:      req.VCPUs,
		MemoryMB:   req.MemoryMB,
		Labels:     req.Labels,
		ExecPolicy: execPolicy,
		Mounts:     req.Mounts,
		Ports:      req.Ports,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// If ?wait=true, block until the session is ready.
	if r.URL.Query().Get("wait") == "true" && !sess.Ready {
		sess, err = s.sessionManager.WaitForReady(r.Context(), sess.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessionManager.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	var filter store.SessionFilter
	if status := r.URL.Query().Get("status"); status != "" {
		st := loka.SessionStatus(status)
		filter.Status = &st
	}
	if workerID := r.URL.Query().Get("worker_id"); workerID != "" {
		filter.WorkerID = &workerID
	}

	sessions, err := s.sessionManager.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []*loka.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": sessions,
		"total":    len(sessions),
	})
}

func (s *Server) destroySession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.sessionManager.Destroy(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessionManager.Pause(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessionManager.Resume(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) idleSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessionManager.Idle(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

type setModeReq struct {
	Mode string `json:"mode"`
}

func (s *Server) setSessionMode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req setModeReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sess, err := s.sessionManager.SetMode(r.Context(), id, loka.ExecMode(req.Mode))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// getWhitelist returns the session's command whitelist and blocklist.
func (s *Server) getWhitelist(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.sessionManager.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed_commands": sess.ExecPolicy.AllowedCommands,
		"blocked_commands": sess.ExecPolicy.BlockedCommands,
	})
}

// updateWhitelist adds/removes commands from the whitelist and blocklist.
func (s *Server) updateWhitelist(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Add    []string `json:"add"`    // Add to allowed.
		Remove []string `json:"remove"` // Remove from allowed.
		Block  []string `json:"block"`  // Add to blocked.
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	sess, err := s.sessionManager.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	allowed := make(map[string]bool)
	for _, c := range sess.ExecPolicy.AllowedCommands {
		allowed[c] = true
	}
	for _, c := range req.Add {
		allowed[c] = true
	}
	for _, c := range req.Remove {
		delete(allowed, c)
	}
	sess.ExecPolicy.AllowedCommands = make([]string, 0, len(allowed))
	for c := range allowed {
		sess.ExecPolicy.AllowedCommands = append(sess.ExecPolicy.AllowedCommands, c)
	}

	blocked := make(map[string]bool)
	for _, c := range sess.ExecPolicy.BlockedCommands {
		blocked[c] = true
	}
	for _, c := range req.Block {
		blocked[c] = true
	}
	sess.ExecPolicy.BlockedCommands = make([]string, 0, len(blocked))
	for c := range blocked {
		sess.ExecPolicy.BlockedCommands = append(sess.ExecPolicy.BlockedCommands, c)
	}

	sess.UpdatedAt = time.Now()
	s.store.Sessions().Update(r.Context(), sess)

	writeJSON(w, http.StatusOK, map[string]any{
		"allowed_commands": sess.ExecPolicy.AllowedCommands,
		"blocked_commands": sess.ExecPolicy.BlockedCommands,
	})
}
