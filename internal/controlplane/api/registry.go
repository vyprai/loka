package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/registry"
)

// RegistryServer implements OCI Distribution Spec endpoints.
type RegistryServer struct {
	store   *registry.Store
	logger  interface{ Info(string, ...any) }
	apiKey  string

	// Upload tracking for chunked uploads.
	uploads   map[string]*uploadState
	uploadsMu sync.Mutex
}

const (
	maxUploadTotalSize = 5 * 1024 * 1024 * 1024 // 5GB max per upload
	maxUploadAge       = 1 * time.Hour
)

type uploadState struct {
	mu        sync.Mutex
	buf       []byte
	createdAt time.Time
}

// NewRegistryServer creates a new OCI registry API server.
func NewRegistryServer(store *registry.Store, logger interface{ Info(string, ...any) }, apiKey string) *RegistryServer {
	return &RegistryServer{
		store:   store,
		logger:  logger,
		apiKey:  apiKey,
		uploads: make(map[string]*uploadState),
	}
}

// Handler returns the http.Handler for the registry server.
func (rs *RegistryServer) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)

	// OCI Distribution Spec routes.
	// /v2/ — API version check.
	r.Get("/v2/", rs.registryPing)
	r.Get("/v2", rs.registryPing)

	// /v2/_catalog — list repositories.
	r.Get("/v2/_catalog", rs.catalog)

	// Routes with {name} that may contain slashes (e.g., library/node).
	// Chi doesn't support catch-all in the middle, so we use a wildcard route.
	r.Route("/v2", func(r chi.Router) {
		// Tags list: GET /v2/{name}/tags/list
		r.Get("/*", rs.routeByPath)
		r.Head("/*", rs.routeByPath)
		r.Put("/*", rs.routeByPath)
		r.Post("/*", rs.routeByPath)
		r.Patch("/*", rs.routeByPath)
	})

	return r
}

// routeByPath dispatches requests based on the URL path structure.
// This handles the OCI Distribution Spec routes where {name} may contain slashes.
func (rs *RegistryServer) routeByPath(w http.ResponseWriter, r *http.Request) {
	path := chi.URLParam(r, "*")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Parse the path to extract name and action.
	// Patterns:
	//   {name}/manifests/{reference}
	//   {name}/blobs/{digest}
	//   {name}/blobs/uploads/
	//   {name}/blobs/uploads/{uuid}
	//   {name}/tags/list

	if idx := strings.Index(path, "/manifests/"); idx >= 0 {
		name := path[:idx]
		ref := path[idx+len("/manifests/"):]
		switch r.Method {
		case http.MethodGet:
			rs.getManifest(w, r, name, ref)
		case http.MethodHead:
			rs.headManifest(w, r, name, ref)
		case http.MethodPut:
			rs.putManifest(w, r, name, ref)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if idx := strings.Index(path, "/blobs/uploads/"); idx >= 0 {
		name := path[:idx]
		uploadID := path[idx+len("/blobs/uploads/"):]
		if uploadID == "" {
			// POST /v2/{name}/blobs/uploads/ — start upload.
			if r.Method == http.MethodPost {
				rs.startUpload(w, r, name)
				return
			}
		} else {
			switch r.Method {
			case http.MethodPatch:
				rs.uploadChunk(w, r, name, uploadID)
			case http.MethodPut:
				rs.completeUpload(w, r, name, uploadID)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
	}

	// POST with path ending in /blobs/uploads (no trailing slash).
	if strings.HasSuffix(path, "/blobs/uploads") && r.Method == http.MethodPost {
		name := strings.TrimSuffix(path, "/blobs/uploads")
		rs.startUpload(w, r, name)
		return
	}

	if idx := strings.Index(path, "/blobs/"); idx >= 0 {
		name := path[:idx]
		digest := path[idx+len("/blobs/"):]
		// Skip if this is an upload path.
		if !strings.HasPrefix(digest, "uploads") {
			switch r.Method {
			case http.MethodGet:
				rs.getBlob(w, r, name, digest)
			case http.MethodHead:
				rs.headBlob(w, r, name, digest)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
	}

	if idx := strings.Index(path, "/tags/list"); idx >= 0 {
		name := path[:idx]
		rs.listTags(w, r, name)
		return
	}

	http.NotFound(w, r)
}

// registryPing handles GET /v2/ — OCI version check.
func (rs *RegistryServer) registryPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
}

// getManifest handles GET /v2/{name}/manifests/{reference}.
func (rs *RegistryServer) getManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	manifest, err := rs.store.GetManifest(r.Context(), name, reference)
	if err != nil {
		writeRegistryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found")
		return
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "failed to serialize manifest")
		return
	}

	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = registry.MediaTypeOCIManifest
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// headManifest handles HEAD /v2/{name}/manifests/{reference}.
func (rs *RegistryServer) headManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	manifest, err := rs.store.GetManifest(r.Context(), name, reference)
	if err != nil {
		writeRegistryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found")
		return
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "failed to serialize manifest")
		return
	}

	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = registry.MediaTypeOCIManifest
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
}

// putManifest handles PUT /v2/{name}/manifests/{reference}.
func (rs *RegistryServer) putManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	r.Body = http.MaxBytesReader(nil, r.Body, 10*1024*1024) // 10MB max for manifests
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "MANIFEST_INVALID", "failed to read body")
		return
	}

	var manifest registry.OCIManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		writeRegistryError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid manifest JSON")
		return
	}

	if err := rs.store.PutManifest(r.Context(), name, reference, &manifest); err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "failed to store manifest")
		return
	}

	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusCreated)
}

// getBlob handles GET /v2/{name}/blobs/{digest}.
func (rs *RegistryServer) getBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	reader, size, err := rs.store.GetBlob(r.Context(), digest)
	if err != nil {
		writeRegistryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
		return
	}
	defer reader.Close()

	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

// headBlob handles HEAD /v2/{name}/blobs/{digest}.
func (rs *RegistryServer) headBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if !rs.store.HasBlob(r.Context(), digest) {
		writeRegistryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
		return
	}

	// Get size.
	reader, size, err := rs.store.GetBlob(r.Context(), digest)
	if err != nil {
		writeRegistryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
		return
	}
	reader.Close()

	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Docker-Content-Digest", digest)
	if size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	w.WriteHeader(http.StatusOK)
}

// startUpload handles POST /v2/{name}/blobs/uploads/ — initiate upload.
func (rs *RegistryServer) startUpload(w http.ResponseWriter, r *http.Request, name string) {
	uploadID := uuid.New().String()

	rs.uploadsMu.Lock()
	rs.uploads[uploadID] = &uploadState{createdAt: time.Now()}
	rs.uploadsMu.Unlock()

	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadID))
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
}

// uploadChunk handles PATCH /v2/{name}/blobs/uploads/{uuid} — upload chunk.
func (rs *RegistryServer) uploadChunk(w http.ResponseWriter, r *http.Request, name, uploadID string) {
	rs.uploadsMu.Lock()
	state, ok := rs.uploads[uploadID]
	rs.uploadsMu.Unlock()

	if !ok {
		writeRegistryError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}

	// Reject expired uploads.
	if time.Since(state.createdAt) > maxUploadAge {
		writeRegistryError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "upload session expired")
		return
	}

	r.Body = http.MaxBytesReader(nil, r.Body, 1*1024*1024*1024) // 1GB max per chunk
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "failed to read chunk")
		return
	}

	state.mu.Lock()
	if int64(len(state.buf))+int64(len(data)) > maxUploadTotalSize {
		state.mu.Unlock()
		writeRegistryError(w, http.StatusRequestEntityTooLarge, "BLOB_UPLOAD_INVALID", "upload exceeds 5GB limit")
		return
	}
	state.buf = append(state.buf, data...)
	offset := len(state.buf)
	state.mu.Unlock()

	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadID))
	w.Header().Set("Docker-Upload-UUID", uploadID)
	w.Header().Set("Range", fmt.Sprintf("0-%d", offset-1))
	w.WriteHeader(http.StatusAccepted)
}

// completeUpload handles PUT /v2/{name}/blobs/uploads/{uuid} — complete upload.
func (rs *RegistryServer) completeUpload(w http.ResponseWriter, r *http.Request, name, uploadID string) {
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		writeRegistryError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest parameter required")
		return
	}

	rs.uploadsMu.Lock()
	state, ok := rs.uploads[uploadID]
	if ok {
		delete(rs.uploads, uploadID)
	}
	rs.uploadsMu.Unlock()

	if !ok {
		writeRegistryError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}

	// Read any remaining body data.
	r.Body = http.MaxBytesReader(nil, r.Body, 1*1024*1024*1024) // 1GB max
	remaining, err := io.ReadAll(r.Body)
	if err == nil && len(remaining) > 0 {
		state.buf = append(state.buf, remaining...)
	}

	// Store the blob.
	reader := strings.NewReader(string(state.buf))
	if err := rs.store.PutBlob(r.Context(), digest, reader, int64(len(state.buf))); err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "failed to store blob")
		return
	}

	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, digest))
	w.WriteHeader(http.StatusCreated)
}

// catalog handles GET /v2/_catalog.
func (rs *RegistryServer) catalog(w http.ResponseWriter, r *http.Request) {
	repos, err := rs.store.ListRepositories(r.Context())
	if err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "failed to list repositories")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	json.NewEncoder(w).Encode(map[string][]string{
		"repositories": repos,
	})
}

// listTags handles GET /v2/{name}/tags/list.
func (rs *RegistryServer) listTags(w http.ResponseWriter, r *http.Request, name string) {
	tags, err := rs.store.ListTags(r.Context(), name)
	if err != nil {
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	json.NewEncoder(w).Encode(map[string]any{
		"name": name,
		"tags": tags,
	})
}

// writeRegistryError writes an OCI Distribution Spec error response.
func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{
			{
				"code":    code,
				"message": message,
			},
		},
	})
}
