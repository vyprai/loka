package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// listArtifacts returns files changed in a session relative to the base image.
// GET /sessions/{id}/artifacts?checkpoint={cpId}
func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	checkpointID := r.URL.Query().Get("checkpoint")

	artifacts, err := s.sessionManager.ListArtifacts(r.Context(), sessionID, checkpointID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"artifacts": artifacts,
		"total":     len(artifacts),
	})
}

// downloadArtifact downloads a single file or a tar archive of all artifacts.
// GET /sessions/{id}/artifacts/download?path=/workspace/output.csv
// GET /sessions/{id}/artifacts/download?format=tar
func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	path := r.URL.Query().Get("path")
	format := r.URL.Query().Get("format")

	if format == "tar" {
		data, err := s.sessionManager.DownloadArtifactsTar(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", "attachment; filename=\"artifacts.tar\"")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	if path == "" {
		writeError(w, http.StatusBadRequest, "query param 'path' or 'format=tar' is required")
		return
	}

	data, err := s.sessionManager.DownloadArtifact(r.Context(), sessionID, path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(path)+"\"")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// listCheckpointArtifacts returns files changed up to a specific checkpoint.
// GET /sessions/{id}/checkpoints/{cpId}/artifacts
func (s *Server) listCheckpointArtifacts(w http.ResponseWriter, r *http.Request) {
	sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	cpID := chi.URLParam(r, "cpId")

	artifacts, err := s.sessionManager.ListArtifacts(r.Context(), sessionID, cpID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"artifacts": artifacts,
		"total":     len(artifacts),
	})
}

// sanitizeFilename extracts the base name from a path for Content-Disposition.
func sanitizeFilename(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
