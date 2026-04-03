package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/loka"
)

type createVolumeReq struct {
	Name         string `json:"name"`
	Type         string `json:"type"`                    // "block" or "object" (default: "block")
	Bucket       string `json:"bucket,omitempty"`        // Object volume: user's own bucket (direct mode)
	Prefix       string `json:"prefix,omitempty"`
	Region       string `json:"region,omitempty"`
	Credentials  string `json:"credentials,omitempty"`
	MaxSizeBytes int64  `json:"max_size_bytes,omitempty"` // Optional max size (0 = unlimited)
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

	var vol *loka.VolumeRecord
	var err error

	switch req.Type {
	case "object":
		vol, err = s.volumeManager.CreateObject(r.Context(), req.Name, req.Bucket, req.Prefix, req.Region, req.Credentials, req.MaxSizeBytes)
	default:
		vol, err = s.volumeManager.CreateBlock(r.Context(), req.Name, req.MaxSizeBytes)
	}

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

	resp := map[string]any{
		"volume": vol,
	}

	// For object volumes with objstore, include file listing.
	if vol.Type == loka.VolumeTypeObject && vol.Bucket != "" {
		files, _ := s.volumeManager.ListFiles(r.Context(), name)
		var totalSize int64
		fileCount := 0
		if files != nil {
			fileCount = len(files)
			for _, f := range files {
				totalSize += f.Size
			}
		}
		resp["file_count"] = fileCount
		resp["total_size"] = totalSize
	}

	// For block/loka-managed volumes, include placement info.
	if vol.IsLokaManaged() {
		resp["primary_worker_id"] = vol.PrimaryWorkerID
		resp["replica_worker_ids"] = vol.ReplicaWorkerIDs
		resp["status"] = vol.Status
	}

	writeJSON(w, http.StatusOK, resp)
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

// ── Volume Locks ────────────────────────────────────────

// acquireVolumeLock acquires a file lock on a volume.
// POST /api/v1/volumes/{name}/lock
func (s *Server) acquireVolumeLock(w http.ResponseWriter, r *http.Request) {
	if s.lockManager == nil {
		writeError(w, http.StatusServiceUnavailable, "lock manager not available")
		return
	}

	volume := chi.URLParam(r, "name")
	var req struct {
		Path      string `json:"path"`
		WorkerID  string `json:"worker_id"`
		Exclusive bool   `json:"exclusive"`
		TTL       int    `json:"ttl"` // Seconds.
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	ttl := time.Duration(req.TTL) * time.Second
	if err := s.lockManager.Acquire(volume, req.Path, req.WorkerID, req.Exclusive, ttl); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "locked"})
}

// releaseVolumeLock releases a file lock on a volume.
// DELETE /api/v1/volumes/{name}/lock
func (s *Server) releaseVolumeLock(w http.ResponseWriter, r *http.Request) {
	if s.lockManager == nil {
		writeError(w, http.StatusServiceUnavailable, "lock manager not available")
		return
	}

	volume := chi.URLParam(r, "name")
	var req struct {
		Path     string `json:"path"`
		WorkerID string `json:"worker_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.lockManager.Release(volume, req.Path, req.WorkerID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "unlocked"})
}

// listVolumeLocks returns all active locks for a volume.
// GET /api/v1/volumes/{name}/locks
func (s *Server) listVolumeLocks(w http.ResponseWriter, r *http.Request) {
	if s.lockManager == nil {
		writeError(w, http.StatusServiceUnavailable, "lock manager not available")
		return
	}

	volume := chi.URLParam(r, "name")
	locks := s.lockManager.ListLocks(volume)
	writeJSON(w, http.StatusOK, map[string]any{"locks": locks})
}
