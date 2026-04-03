// Package volrepl provides worker-to-worker volume replication for block volumes.
// The primary worker pushes changes to replica workers via HTTP; replicas can
// also pull manifests and individual files on demand.
package volrepl

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/vyprai/loka/internal/worker/volsync"
)

// Handler serves volume data to peer workers over HTTP.
type Handler struct {
	dataDir string // volumes at {dataDir}/volumes/{name}/
	logger  *slog.Logger
}

// NewHandler creates a volume replication HTTP handler.
func NewHandler(dataDir string, logger *slog.Logger) *Handler {
	return &Handler{dataDir: dataDir, logger: logger}
}

// Register mounts the replication routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /volrepl/{name}/manifest", h.getManifest)
	mux.HandleFunc("GET /volrepl/{name}/file", h.getFile)
	mux.HandleFunc("POST /volrepl/{name}/file", h.putFile)
	mux.HandleFunc("DELETE /volrepl/{name}/file", h.deleteFile)
}

// getManifest returns the current manifest for a volume.
func (h *Handler) getManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	volDir := filepath.Join(h.dataDir, "volumes", name)

	if _, err := os.Stat(volDir); os.IsNotExist(err) {
		http.Error(w, "volume not found", http.StatusNotFound)
		return
	}

	manifest := volsync.BuildLocalManifest(volDir)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}

// getFile streams a single file from the volume.
func (h *Handler) getFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		http.Error(w, "missing path query parameter", http.StatusBadRequest)
		return
	}

	// Prevent path traversal.
	clean := filepath.Clean(relPath)
	if strings.Contains(clean, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(h.dataDir, "volumes", name, clean)
	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, clean, info.ModTime(), f)
}

// putFile receives a file from the primary worker.
func (h *Handler) putFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		http.Error(w, "missing path query parameter", http.StatusBadRequest)
		return
	}

	clean := filepath.Clean(relPath)
	if strings.Contains(clean, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	volDir := filepath.Join(h.dataDir, "volumes", name)
	os.MkdirAll(volDir, 0o755)

	fullPath := filepath.Join(volDir, clean)
	os.MkdirAll(filepath.Dir(fullPath), 0o755)

	// Atomic write via temp file.
	tmpPath := fullPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// deleteFile removes a file on the replica.
func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		http.Error(w, "missing path query parameter", http.StatusBadRequest)
		return
	}

	clean := filepath.Clean(relPath)
	if strings.Contains(clean, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(h.dataDir, "volumes", name, clean)
	os.Remove(fullPath)
	w.WriteHeader(http.StatusNoContent)
}
