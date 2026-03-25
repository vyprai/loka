package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/loka"
)

type createVolumeReq struct {
	Name string `json:"name"`
}

// createVolume creates a new named volume.
// POST /api/v1/volumes
func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var req createVolumeReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	vol, err := s.volumeManager.Create(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, vol)
}

// listVolumes returns all named volumes.
// GET /api/v1/volumes
func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	volumes, err := s.volumeManager.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if volumes == nil {
		volumes = []*loka.VolumeRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volumes": volumes,
		"total":   len(volumes),
	})
}

// getVolume returns details of a named volume.
// GET /api/v1/volumes/{name}
func (s *Server) getVolume(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	vol, err := s.volumeManager.Get(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Also fetch file list from objstore for inspect.
	files, _ := s.volumeManager.ListFiles(r.Context(), name)
	var totalSize int64
	fileCount := 0
	if files != nil {
		fileCount = len(files)
		for _, f := range files {
			totalSize += f.Size
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"volume":     vol,
		"file_count": fileCount,
		"total_size": totalSize,
	})
}

// deleteVolume deletes a named volume.
// DELETE /api/v1/volumes/{name}
func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.volumeManager.Delete(r.Context(), name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
