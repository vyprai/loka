package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/registry"
)

// registerRegistryRoutes adds image registry management routes to the main API.
func (s *Server) registerRegistryRoutes(r chi.Router) {
	// Pull image via registry.
	r.Post("/images/registry/pull", s.registryPull)

	// Catalog.
	r.Get("/images/registry/catalog", s.registryCatalog)

	// Blobs listing.
	r.Get("/images/registry/blobs", s.registryListBlobs)

	// Tags: GET /api/v1/images/registry/tags/{name}
	// Name may contain slashes, so use wildcard.
	r.Get("/images/registry/tags/*", s.registryListTags)

	// Manifests: GET/DELETE /api/v1/images/registry/manifests/{name}/{reference}
	r.Get("/images/registry/manifests/*", s.registryGetManifest)
	r.Delete("/images/registry/manifests/*", s.registryDeleteManifest)
}

func (s *Server) registryPull(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	var req struct {
		Image    string `json:"image"`
		Token    string `json:"token,omitempty"`
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Image == "" {
		writeError(w, http.StatusBadRequest, "image reference required")
		return
	}

	auth := &registry.AuthConfig{
		Token:    req.Token,
		Username: req.Username,
		Password: req.Password,
	}

	manifest, err := registry.Pull(r.Context(), req.Image, s.registryStore, auth)
	if err != nil {
		s.logger.Error("registry pull failed", "image", req.Image, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("pull failed: %v", err))
		return
	}

	_, repo, tag := registry.ParseReference(req.Image)
	s.logger.Info("image pulled", "repo", repo, "tag", tag)
	writeJSON(w, http.StatusOK, map[string]any{
		"message":  fmt.Sprintf("pulled %s:%s", repo, tag),
		"manifest": manifest,
	})
}

func (s *Server) registryCatalog(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	repos, err := s.registryStore.ListRepositories(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if repos == nil {
		repos = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"repositories": repos})
}

func (s *Server) registryListBlobs(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	blobs, err := s.registryStore.ListBlobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if blobs == nil {
		blobs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"blobs": blobs})
}

func (s *Server) registryListTags(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	name := chi.URLParam(r, "*")
	if name == "" {
		writeError(w, http.StatusBadRequest, "repository name required")
		return
	}

	tags, err := s.registryStore.ListTags(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "tags": tags})
}

func (s *Server) registryGetManifest(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	path := chi.URLParam(r, "*")
	name, ref := splitManifestPath(path)
	if name == "" || ref == "" {
		writeError(w, http.StatusBadRequest, "path must be {name}/{reference}")
		return
	}

	manifest, err := s.registryStore.GetManifest(r.Context(), name, ref)
	if err != nil {
		writeError(w, http.StatusNotFound, "manifest not found")
		return
	}

	data, _ := json.Marshal(manifest)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) registryDeleteManifest(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	path := chi.URLParam(r, "*")
	name, ref := splitManifestPath(path)
	if name == "" || ref == "" {
		writeError(w, http.StatusBadRequest, "path must be {name}/{reference}")
		return
	}

	// Get manifest first to find associated blobs.
	manifest, err := s.registryStore.GetManifest(r.Context(), name, ref)
	if err != nil {
		writeError(w, http.StatusNotFound, "manifest not found")
		return
	}

	// Delete manifest.
	if err := s.registryStore.DeleteManifest(r.Context(), name, ref); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Attempt to clean up unreferenced blobs (best effort).
	// Delete config and layer blobs if they are not referenced by other manifests.
	_ = s.registryStore.DeleteBlob(r.Context(), manifest.Config.Digest)
	for _, layer := range manifest.Layers {
		_ = s.registryStore.DeleteBlob(r.Context(), layer.Digest)
	}

	w.WriteHeader(http.StatusNoContent)
}

// splitManifestPath splits "library/node/20-slim" into ("library/node", "20-slim").
// The last path segment is the reference; everything before is the name.
func splitManifestPath(path string) (name, ref string) {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "", ""
	}
	return path[:idx], path[idx+1:]
}

// registryBlobGet handles direct blob download from the management API.
func (s *Server) registryBlobGet(w http.ResponseWriter, r *http.Request) {
	if s.registryStore == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not configured")
		return
	}

	digest := chi.URLParam(r, "*")
	reader, size, err := s.registryStore.GetBlob(r.Context(), digest)
	if err != nil {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	if size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}
