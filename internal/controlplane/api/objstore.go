package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/objstore"
)

func extractBucketKey(r *http.Request) (string, string) {
	bucket, _ := url.PathUnescape(chi.URLParam(r, "bucket"))
	// chi wildcard captures everything after /objects/{bucket}/
	key := chi.URLParam(r, "*")
	key, _ = url.PathUnescape(key)
	key = strings.TrimPrefix(key, "/")
	return bucket, key
}

func (s *Server) objStorePut(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		writeError(w, http.StatusServiceUnavailable, "object store not configured")
		return
	}
	bucket, key := extractBucketKey(r)
	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key required")
		return
	}

	if err := s.objStore.Put(r.Context(), bucket, key, r.Body, r.ContentLength); err != nil {
		s.logger.Error("objstore put failed", "bucket", bucket, "key", key, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) objStoreGet(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		writeError(w, http.StatusServiceUnavailable, "object store not configured")
		return
	}
	bucket, key := extractBucketKey(r)
	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "bucket and key required")
		return
	}

	// Check if presign requested.
	if r.URL.Query().Get("presign") == "true" {
		expiryStr := r.URL.Query().Get("expiry")
		expiry, err := time.ParseDuration(expiryStr)
		if err != nil {
			expiry = 15 * time.Minute
		}
		presignURL, err := s.objStore.GetPresignedURL(r.Context(), bucket, key, expiry)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": presignURL})
		return
	}

	reader, err := s.objStore.Get(r.Context(), bucket, key)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("not found: %s/%s", bucket, key))
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		defer f.Flush()
	}
	// Stream the object directly — no buffering.
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
}

func (s *Server) objStoreHead(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	bucket, key := extractBucketKey(r)

	exists, err := s.objStore.Exists(r.Context(), bucket, key)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) objStoreDelete(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		writeError(w, http.StatusServiceUnavailable, "object store not configured")
		return
	}
	bucket, key := extractBucketKey(r)

	if err := s.objStore.Delete(r.Context(), bucket, key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) objStoreList(w http.ResponseWriter, r *http.Request) {
	if s.objStore == nil {
		writeError(w, http.StatusServiceUnavailable, "object store not configured")
		return
	}
	bucket, _ := url.PathUnescape(chi.URLParam(r, "bucket"))
	prefix := r.URL.Query().Get("prefix")

	objects, err := s.objStore.List(r.Context(), bucket, prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if objects == nil {
		objects = []objstore.ObjectInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(objects)
}
