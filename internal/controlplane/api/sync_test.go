package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vyprai/loka/internal/loka"
)

// createRunningSession creates a basic running session for sync tests.
func createRunningSession(t *testing.T, ts *testServer) string {
	t.Helper()
	ts.registerTestWorker(t)

	payload := map[string]any{
		"name":  "sync-test",
		"image": "alpine:latest",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions", payload, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess loka.Session
	decodeBody(t, rec, &sess)
	return sess.ID
}

func TestSyncMount_MissingMountPath(t *testing.T) {
	ts := setupTestServer(t)
	sessionID := createRunningSession(t, ts)

	body := map[string]any{
		"direction": "push",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/sync", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	decodeBody(t, rec, &errBody)
	if errBody["error"] != "mount_path is required" {
		t.Errorf("unexpected error message: %q", errBody["error"])
	}
}

func TestSyncMount_MissingDirection(t *testing.T) {
	ts := setupTestServer(t)
	sessionID := createRunningSession(t, ts)

	body := map[string]any{
		"mount_path": "/data",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/sync", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	decodeBody(t, rec, &errBody)
	if errBody["error"] != "direction is required (push or pull)" {
		t.Errorf("unexpected error message: %q", errBody["error"])
	}
}

func TestSyncMount_InvalidDirection(t *testing.T) {
	ts := setupTestServer(t)
	sessionID := createRunningSession(t, ts)

	body := map[string]any{
		"mount_path": "/data",
		"direction":  "sideways",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/sync", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	decodeBody(t, rec, &errBody)
	if errBody["error"] != "direction must be 'push' or 'pull'" {
		t.Errorf("unexpected error message: %q", errBody["error"])
	}
}

func TestSyncMount_SessionNotFound(t *testing.T) {
	ts := setupTestServer(t)

	body := map[string]any{
		"mount_path": "/data",
		"direction":  "push",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/nonexistent-id/sync", body, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSyncMount_SessionNotRunning(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	// Create a session and then pause it.
	payload := map[string]any{
		"name":  "paused-sync",
		"image": "alpine:latest",
	}
	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions", payload, nil)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var sess loka.Session
	decodeBody(t, createRec, &sess)

	// Pause the session.
	pauseRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sess.ID+"/pause", nil, nil)
	if pauseRec.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d: %s", pauseRec.Code, pauseRec.Body.String())
	}

	body := map[string]any{
		"mount_path": "/data",
		"direction":  "push",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sess.ID+"/sync", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSyncMount_MountNotFound verifies that a valid sync request to a running
// session returns 404 when the requested mount path does not exist on the
// session. This exercises the mount-lookup path in the handler.
func TestSyncMount_MountNotFound(t *testing.T) {
	ts := setupTestServer(t)
	sessionID := createRunningSession(t, ts)

	body := map[string]any{
		"mount_path": "/nonexistent-mount",
		"direction":  "push",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/sync", body, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing mount, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	decodeBody(t, rec, &errBody)
	expected := `no mount at path "/nonexistent-mount"`
	if errBody["error"] != expected {
		t.Errorf("expected error %q, got %q", expected, errBody["error"])
	}
}

// TestSyncMount_InvalidRequestBody verifies that a malformed JSON body returns 400.
func TestSyncMount_InvalidRequestBody(t *testing.T) {
	ts := setupTestServer(t)
	sessionID := createRunningSession(t, ts)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessionID+"/sync",
		bytes.NewReader([]byte("not-json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSyncMount_ValidationOrder verifies that validation checks run in order:
// mount_path first, then direction.
func TestSyncMount_ValidationOrder(t *testing.T) {
	ts := setupTestServer(t)
	sessionID := createRunningSession(t, ts)

	// Both mount_path and direction missing -- mount_path error comes first.
	body := map[string]any{}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+sessionID+"/sync", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var errBody map[string]string
	decodeBody(t, rec, &errBody)
	if errBody["error"] != "mount_path is required" {
		t.Errorf("expected mount_path error first, got %q", errBody["error"])
	}
}
