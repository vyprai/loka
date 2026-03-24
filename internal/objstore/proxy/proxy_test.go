package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/objstore"
	localobjstore "github.com/vyprai/loka/internal/objstore/local"
)

// testObjStoreServer creates a simple HTTP server that wraps a local objstore.
func testObjStoreServer(t *testing.T) (*httptest.Server, objstore.ObjectStore) {
	t.Helper()
	store, err := localobjstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()

	// PUT /api/internal/objstore/objects/{bucket}/{key...}
	mux.HandleFunc("PUT /api/internal/objstore/objects/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/internal/objstore/objects/"), "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad path", 400)
			return
		}
		if err := store.Put(r.Context(), parts[0], parts[1], r.Body, r.ContentLength); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	})

	// GET /api/internal/objstore/objects/{bucket}/{key...}
	mux.HandleFunc("GET /api/internal/objstore/objects/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/internal/objstore/objects/"), "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad path", 400)
			return
		}
		reader, err := store.Get(r.Context(), parts[0], parts[1])
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		defer reader.Close()
		w.WriteHeader(200)
		io.Copy(w, reader)
	})

	// HEAD /api/internal/objstore/objects/{bucket}/{key...}
	mux.HandleFunc("HEAD /api/internal/objstore/objects/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/internal/objstore/objects/"), "/", 2)
		if len(parts) < 2 {
			w.WriteHeader(400)
			return
		}
		exists, _ := store.Exists(r.Context(), parts[0], parts[1])
		if exists {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(404)
		}
	})

	// DELETE /api/internal/objstore/objects/{bucket}/{key...}
	mux.HandleFunc("DELETE /api/internal/objstore/objects/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/internal/objstore/objects/"), "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad path", 400)
			return
		}
		store.Delete(r.Context(), parts[0], parts[1])
		w.WriteHeader(204)
	})

	// GET /api/internal/objstore/list/{bucket}?prefix=...
	mux.HandleFunc("GET /api/internal/objstore/list/", func(w http.ResponseWriter, r *http.Request) {
		bucket := strings.TrimPrefix(r.URL.Path, "/api/internal/objstore/list/")
		prefix := r.URL.Query().Get("prefix")
		objects, _ := store.List(r.Context(), bucket, prefix)
		if objects == nil {
			objects = []objstore.ObjectInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(objects)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, store
}

func TestProxy_PutGetRoundTrip(t *testing.T) {
	ts, _ := testObjStoreServer(t)
	proxy := New(Config{BaseURL: ts.URL})
	ctx := context.Background()

	data := []byte("hello proxy world")
	err := proxy.Put(ctx, "test-bucket", "greeting.txt", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := proxy.Get(ctx, "test-bucket", "greeting.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Errorf("round trip mismatch: got %q, want %q", got, data)
	}
}

func TestProxy_Exists(t *testing.T) {
	ts, _ := testObjStoreServer(t)
	proxy := New(Config{BaseURL: ts.URL})
	ctx := context.Background()

	exists, err := proxy.Exists(ctx, "bucket", "nonexistent")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("expected not exists")
	}

	proxy.Put(ctx, "bucket", "key", bytes.NewReader([]byte("data")), 4)

	exists, err = proxy.Exists(ctx, "bucket", "key")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("expected exists")
	}
}

func TestProxy_Delete(t *testing.T) {
	ts, _ := testObjStoreServer(t)
	proxy := New(Config{BaseURL: ts.URL})
	ctx := context.Background()

	proxy.Put(ctx, "bucket", "delete-me", bytes.NewReader([]byte("data")), 4)
	proxy.Delete(ctx, "bucket", "delete-me")

	exists, _ := proxy.Exists(ctx, "bucket", "delete-me")
	if exists {
		t.Error("expected deleted")
	}
}

func TestProxy_List(t *testing.T) {
	ts, _ := testObjStoreServer(t)
	proxy := New(Config{BaseURL: ts.URL})
	ctx := context.Background()

	proxy.Put(ctx, "bucket", "a/1.txt", bytes.NewReader([]byte("a1")), 2)
	proxy.Put(ctx, "bucket", "a/2.txt", bytes.NewReader([]byte("a2")), 2)
	proxy.Put(ctx, "bucket", "b/3.txt", bytes.NewReader([]byte("b3")), 2)

	objects, err := proxy.List(ctx, "bucket", "a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 2 {
		t.Errorf("expected 2 objects, got %d", len(objects))
	}
}

func TestProxy_GetNotFound(t *testing.T) {
	ts, _ := testObjStoreServer(t)
	proxy := New(Config{BaseURL: ts.URL})
	ctx := context.Background()

	_, err := proxy.Get(ctx, "bucket", "nope")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestProxy_SetBaseURL(t *testing.T) {
	ts, _ := testObjStoreServer(t)
	proxy := New(Config{BaseURL: "http://wrong:9999"})
	proxy.SetBaseURL(ts.URL)
	ctx := context.Background()

	proxy.Put(ctx, "bucket", "key", bytes.NewReader([]byte("data")), 4)

	exists, _ := proxy.Exists(ctx, "bucket", "key")
	if !exists {
		t.Error("expected exists after SetBaseURL")
	}
}

// Ensure interface compliance.
var _ objstore.ObjectStore = (*Store)(nil)

// Keep time import alive.
var _ = time.Now
