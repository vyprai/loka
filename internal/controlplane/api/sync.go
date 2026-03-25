package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/loka"
)

func (s *Server) syncMount(w http.ResponseWriter, r *http.Request) {
	sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req loka.SyncRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.MountPath == "" {
		writeError(w, http.StatusBadRequest, "mount_path is required")
		return
	}
	if req.Direction == "" {
		writeError(w, http.StatusBadRequest, "direction is required (push or pull)")
		return
	}
	if req.Direction != loka.SyncPush && req.Direction != loka.SyncPull {
		writeError(w, http.StatusBadRequest, "direction must be 'push' or 'pull'")
		return
	}

	// Verify session exists and is running.
	sess, err := s.sessionManager.Get(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.Status != loka.SessionStatusRunning {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("session is %s, must be running to sync", sess.Status))
		return
	}

	// Find the matching mount.
	var mount *loka.Volume
	for i := range sess.Mounts {
		if sess.Mounts[i].Path == req.MountPath {
			mount = &sess.Mounts[i]
			break
		}
	}
	if mount == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no mount at path %q", req.MountPath))
		return
	}

	if mount.IsReadOnly() && req.Direction == loka.SyncPull {
		// Pull into a read-only mount updates the VM's view — allowed.
	}
	if mount.IsReadOnly() && req.Direction == loka.SyncPush {
		writeError(w, http.StatusBadRequest, "cannot push from a read-only mount")
		return
	}

	// Dispatch sync to the worker via session manager.
	result, err := s.sessionManager.SyncMount(r.Context(), sessionID, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}
