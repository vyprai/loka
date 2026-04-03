package api

import (
	"net/http"
	"testing"
)

func TestAcquireVolumeLock(t *testing.T) {
	ts := setupTestServer(t)

	// Create a volume first.
	volPayload := map[string]any{"name": "lock-vol"}
	volRec := ts.doRequest(t, http.MethodPost, "/api/v1/volumes", volPayload, nil)
	if volRec.Code != http.StatusCreated {
		t.Fatalf("create volume: expected 201, got %d: %s", volRec.Code, volRec.Body.String())
	}

	// Acquire a lock on a file in the volume.
	payload := map[string]any{
		"path":      "/data/file.txt",
		"worker_id": "worker-1",
		"exclusive": true,
		"ttl":       60,
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/volumes/lock-vol/lock", payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeBody(t, rec, &body)
	if body["status"] != "locked" {
		t.Errorf("expected status locked, got %q", body["status"])
	}
}

func TestAcquireVolumeLock_MissingPath(t *testing.T) {
	ts := setupTestServer(t)

	payload := map[string]any{
		"worker_id": "worker-1",
		"exclusive": true,
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/volumes/some-vol/lock", payload, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReleaseVolumeLock(t *testing.T) {
	ts := setupTestServer(t)

	// Acquire a lock first.
	acquirePayload := map[string]any{
		"path":      "/data/file.txt",
		"worker_id": "worker-1",
		"exclusive": true,
		"ttl":       60,
	}
	acquireRec := ts.doRequest(t, http.MethodPost, "/api/v1/volumes/rel-vol/lock", acquirePayload, nil)
	if acquireRec.Code != http.StatusOK {
		t.Fatalf("acquire: expected 200, got %d: %s", acquireRec.Code, acquireRec.Body.String())
	}

	// Release the lock.
	releasePayload := map[string]any{
		"path":      "/data/file.txt",
		"worker_id": "worker-1",
	}
	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/volumes/rel-vol/lock", releasePayload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeBody(t, rec, &body)
	if body["status"] != "unlocked" {
		t.Errorf("expected status unlocked, got %q", body["status"])
	}
}

func TestReleaseVolumeLock_NotHeld(t *testing.T) {
	ts := setupTestServer(t)

	// Releasing a lock that was never acquired is idempotent (returns 200).
	payload := map[string]any{
		"path":      "/data/nonexistent.txt",
		"worker_id": "worker-1",
	}
	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/volumes/no-vol/lock", payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent release), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListVolumeLocks(t *testing.T) {
	ts := setupTestServer(t)

	// Acquire two locks on the same volume.
	for _, path := range []string{"/a.txt", "/b.txt"} {
		payload := map[string]any{
			"path":      path,
			"worker_id": "worker-1",
			"exclusive": true,
			"ttl":       60,
		}
		rec := ts.doRequest(t, http.MethodPost, "/api/v1/volumes/list-vol/lock", payload, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("acquire %s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}

	// List locks.
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/volumes/list-vol/locks", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Locks []map[string]any `json:"locks"`
	}
	decodeBody(t, rec, &body)
	if len(body.Locks) != 2 {
		t.Errorf("expected 2 locks, got %d", len(body.Locks))
	}
}

func TestListVolumeLocks_Empty(t *testing.T) {
	ts := setupTestServer(t)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/volumes/empty-vol/locks", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Locks []map[string]any `json:"locks"`
	}
	decodeBody(t, rec, &body)
	if len(body.Locks) != 0 {
		t.Errorf("expected 0 locks, got %d", len(body.Locks))
	}
}
