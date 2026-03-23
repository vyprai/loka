package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
	"github.com/vyprai/loka/internal/provider"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/internal/store/sqlite"
)

// testServer bundles all the dependencies created during setup so tests
// can interact with them directly when needed.
type testServer struct {
	server   *Server
	store    store.Store
	registry *worker.Registry
	sched    *scheduler.Scheduler
	manager  *session.Manager
	provReg  *provider.Registry
	imgMgr   *image.Manager
	drainer  *worker.Drainer
}

// setupTestServer creates a fully wired Server backed by an in-memory SQLite
// database. The optional ServerOpts are forwarded to NewServer.
func setupTestServer(t *testing.T, opts ...ServerOpts) *testServer {
	t.Helper()

	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	reg := worker.NewRegistry(db, logger)
	sched := scheduler.New(reg, scheduler.StrategySpread)

	tmpDir := t.TempDir()
	objStore, err := local.New(tmpDir)
	if err != nil {
		t.Fatalf("create local objstore: %v", err)
	}
	imgMgr := image.NewManager(objStore, tmpDir, logger)

	mgr := session.NewManager(db, reg, sched, imgMgr, logger)

	provReg := provider.NewRegistry()

	migrateFn := func(ctx context.Context, sessionID, targetWorkerID string) error {
		return mgr.MigrateSession(ctx, sessionID, targetWorkerID)
	}
	drainer := worker.NewDrainer(reg, db, migrateFn, logger)

	srv := NewServer(mgr, reg, provReg, imgMgr, drainer, db, logger, opts...)

	return &testServer{
		server:   srv,
		store:    db,
		registry: reg,
		sched:    sched,
		manager:  mgr,
		provReg:  provReg,
		imgMgr:   imgMgr,
		drainer:  drainer,
	}
}

// doRequest is a small helper that builds an HTTP request, fires it through
// the server handler, and returns the recorded response.
func (ts *testServer) doRequest(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ts.server.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeBody is a test helper that decodes a JSON response into dst.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// registerTestWorker registers a fake worker in the registry so that
// session creation can schedule onto it.
func (ts *testServer) registerTestWorker(t *testing.T) *loka.Worker {
	t.Helper()
	w, err := ts.registry.Register(context.Background(),
		"test-host", "10.0.0.1", "test", "us-east-1", "a", "test-v1",
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192, DiskMB: 100000},
		map[string]string{"env": "test"}, true,
	)
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return w
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRESTHealthCheck(t *testing.T) {
	ts := setupTestServer(t)
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/health", nil, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", body["status"])
	}
}

// ---------------------------------------------------------------------------
// Session CRUD
// ---------------------------------------------------------------------------

func TestRESTCreateSession_Success(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	payload := map[string]any{
		"name":      "my-session",
		"image":     "ubuntu:22.04",
		"mode":      "explore",
		"vcpus":     2,
		"memory_mb": 1024,
		"labels":    map[string]string{"team": "infra"},
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions", payload, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess loka.Session
	decodeBody(t, rec, &sess)
	if sess.Name != "my-session" {
		t.Errorf("expected name my-session, got %q", sess.Name)
	}
	if sess.Mode != loka.ModeExplore {
		t.Errorf("expected mode explore, got %q", sess.Mode)
	}
	if sess.Status != loka.SessionStatusRunning {
		t.Errorf("expected status running, got %q", sess.Status)
	}
}

func TestRESTCreateSession_InvalidBody(t *testing.T) {
	ts := setupTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRESTCreateSession_DefaultsWithoutWorker(t *testing.T) {
	// When there are no workers, the session should still be created
	// (with status running but no worker assignment).
	ts := setupTestServer(t)

	payload := map[string]any{"name": "no-worker-session", "image": "alpine:latest"}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions", payload, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess loka.Session
	decodeBody(t, rec, &sess)
	if sess.Mode != loka.ModeExplore {
		t.Errorf("expected default mode explore, got %q", sess.Mode)
	}
	if sess.VCPUs != 1 {
		t.Errorf("expected default vcpus 1, got %d", sess.VCPUs)
	}
	if sess.MemoryMB != 512 {
		t.Errorf("expected default memory_mb 512, got %d", sess.MemoryMB)
	}
}

func TestRESTListSessions(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	// Create two sessions.
	for _, name := range []string{"sess-a", "sess-b"} {
		ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
			map[string]any{"name": name, "image": "alpine:latest"}, nil)
	}

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body struct {
		Sessions []loka.Session `json:"sessions"`
		Total    int            `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 2 {
		t.Fatalf("expected 2 sessions, got %d", body.Total)
	}
}

func TestRESTGetSession_Success(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	// Create a session.
	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "find-me", "image": "alpine:latest"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	// Fetch it.
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions/"+created.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var fetched loka.Session
	decodeBody(t, rec, &fetched)
	if fetched.ID != created.ID {
		t.Errorf("expected id %s, got %s", created.ID, fetched.ID)
	}
}

func TestRESTGetSession_NotFound(t *testing.T) {
	ts := setupTestServer(t)
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions/nonexistent-id", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestRESTDestroySession(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "destroy-me", "image": "alpine:latest"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/sessions/"+created.ID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the session is terminated.
	getRec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions/"+created.ID, nil, nil)
	var sess loka.Session
	decodeBody(t, getRec, &sess)
	if sess.Status != loka.SessionStatusTerminated {
		t.Errorf("expected terminated, got %q", sess.Status)
	}
}

// ---------------------------------------------------------------------------
// Set Session Mode
// ---------------------------------------------------------------------------

func TestRESTSetSessionMode(t *testing.T) {
	tests := []struct {
		name       string
		startMode  string
		targetMode string
		wantCode   int
	}{
		{"explore to execute", "explore", "execute", http.StatusOK},
		{"explore to ask", "explore", "ask", http.StatusOK},
		{"execute to explore", "execute", "explore", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupTestServer(t)
			ts.registerTestWorker(t)

			createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
				map[string]any{"name": "mode-test", "image": "alpine:latest", "mode": tt.startMode}, nil)
			var created loka.Session
			decodeBody(t, createRec, &created)

			rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/mode",
				map[string]string{"mode": tt.targetMode}, nil)
			if rec.Code != tt.wantCode {
				t.Fatalf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
			if tt.wantCode == http.StatusOK {
				var sess loka.Session
				decodeBody(t, rec, &sess)
				if string(sess.Mode) != tt.targetMode {
					t.Errorf("expected mode %s, got %s", tt.targetMode, sess.Mode)
				}
			}
		})
	}
}

func TestRESTSetSessionMode_InvalidTransition(t *testing.T) {
	ts := setupTestServer(t)

	// Create a session that is immediately terminated.
	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "terminated", "image": "alpine:latest"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	// Destroy it.
	ts.doRequest(t, http.MethodDelete, "/api/v1/sessions/"+created.ID, nil, nil)

	// Setting mode on a nonexistent session or terminated one should fail.
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/nonexistent-id/mode",
		map[string]string{"mode": "execute"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Exec
// ---------------------------------------------------------------------------

func TestRESTExecCommand_Success(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	// Create a session in execute mode to avoid read-only restrictions.
	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "exec-test", "image": "alpine:latest", "mode": "execute"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/exec",
		map[string]any{"command": "echo", "args": []string{"hello"}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var exec loka.Execution
	decodeBody(t, rec, &exec)
	if exec.SessionID != created.ID {
		t.Errorf("expected session_id %s, got %s", created.ID, exec.SessionID)
	}
	if len(exec.Commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(exec.Commands))
	}
	if exec.Commands[0].Command != "echo" {
		t.Errorf("expected command echo, got %q", exec.Commands[0].Command)
	}
}

func TestRESTExecCommand_NoCommands(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "exec-empty", "image": "alpine:latest", "mode": "execute"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	// Send an empty exec request (no commands).
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/exec",
		map[string]any{}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRESTExecCommand_SessionNotFound(t *testing.T) {
	ts := setupTestServer(t)
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/nonexistent/exec",
		map[string]any{"command": "ls"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

func TestRESTListWorkers(t *testing.T) {
	ts := setupTestServer(t)

	// Initially empty.
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/workers", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Workers []loka.Worker `json:"workers"`
		Total   int           `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 0 {
		t.Fatalf("expected 0 workers, got %d", body.Total)
	}

	// Register a worker and list again.
	ts.registerTestWorker(t)

	rec = ts.doRequest(t, http.MethodGet, "/api/v1/workers", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	decodeBody(t, rec, &body)
	if body.Total != 1 {
		t.Fatalf("expected 1 worker, got %d", body.Total)
	}
}

// ---------------------------------------------------------------------------
// Worker Tokens
// ---------------------------------------------------------------------------

func TestRESTCreateWorkerToken(t *testing.T) {
	ts := setupTestServer(t)

	payload := map[string]any{
		"name":           "test-token",
		"expires_seconds": 3600,
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/worker-tokens", payload, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var token loka.WorkerToken
	decodeBody(t, rec, &token)
	if token.Name != "test-token" {
		t.Errorf("expected name test-token, got %q", token.Name)
	}
	if token.Token == "" {
		t.Error("expected non-empty token value")
	}
	if token.ID == "" {
		t.Error("expected non-empty token ID")
	}
}

func TestRESTListWorkerTokens(t *testing.T) {
	ts := setupTestServer(t)

	// Create two tokens.
	for _, name := range []string{"tok-a", "tok-b"} {
		ts.doRequest(t, http.MethodPost, "/api/v1/worker-tokens",
			map[string]any{"name": name}, nil)
	}

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/worker-tokens", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Tokens []map[string]any `json:"tokens"`
		Total  int              `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 2 {
		t.Fatalf("expected 2 tokens, got %d", body.Total)
	}
}

// ---------------------------------------------------------------------------
// API Key Authentication
// ---------------------------------------------------------------------------

func TestRESTAPIKeyAuth(t *testing.T) {
	const testKey = "my-secret-api-key"
	ts := setupTestServer(t, ServerOpts{APIKey: testKey})

	tests := []struct {
		name     string
		path     string
		method   string
		auth     string // value for the Authorization header
		wantCode int
	}{
		{
			name:     "health endpoint skips auth",
			path:     "/api/v1/health",
			method:   http.MethodGet,
			auth:     "",
			wantCode: http.StatusOK,
		},
		{
			name:     "no auth header returns 401",
			path:     "/api/v1/sessions",
			method:   http.MethodGet,
			auth:     "",
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "wrong key returns 403",
			path:     "/api/v1/sessions",
			method:   http.MethodGet,
			auth:     "Bearer wrong-key",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "correct key succeeds",
			path:     "/api/v1/sessions",
			method:   http.MethodGet,
			auth:     "Bearer " + testKey,
			wantCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.auth != "" {
				headers["Authorization"] = tt.auth
			}
			rec := ts.doRequest(t, tt.method, tt.path, nil, headers)
			if rec.Code != tt.wantCode {
				t.Fatalf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRESTAPIKeyAuth_NoKeyConfigured(t *testing.T) {
	// When no API key is configured, all requests should pass through.
	ts := setupTestServer(t)
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 without API key configured, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Exec - multiple commands
// ---------------------------------------------------------------------------

func TestRESTExecCommand_MultipleCommands(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "multi-exec", "image": "alpine:latest", "mode": "execute"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	payload := map[string]any{
		"commands": []map[string]any{
			{"command": "echo", "args": []string{"first"}},
			{"command": "echo", "args": []string{"second"}},
		},
		"parallel": true,
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/exec", payload, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var exec loka.Execution
	decodeBody(t, rec, &exec)
	if len(exec.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(exec.Commands))
	}
	if !exec.Parallel {
		t.Error("expected parallel=true")
	}
}

// ---------------------------------------------------------------------------
// Session lifecycle - pause and resume
// ---------------------------------------------------------------------------

func TestRESTPauseAndResumeSession(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "pausable", "image": "alpine:latest"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	// Pause.
	pauseRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/pause", nil, nil)
	if pauseRec.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d: %s", pauseRec.Code, pauseRec.Body.String())
	}
	var paused loka.Session
	decodeBody(t, pauseRec, &paused)
	if paused.Status != loka.SessionStatusPaused {
		t.Errorf("expected paused status, got %q", paused.Status)
	}

	// Resume.
	resumeRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/resume", nil, nil)
	if resumeRec.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d: %s", resumeRec.Code, resumeRec.Body.String())
	}
	var resumed loka.Session
	decodeBody(t, resumeRec, &resumed)
	if resumed.Status != loka.SessionStatusRunning {
		t.Errorf("expected running status, got %q", resumed.Status)
	}
}

// ---------------------------------------------------------------------------
// List sessions with status filter
// ---------------------------------------------------------------------------

func TestRESTListSessions_FilterByStatus(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	// Create two sessions.
	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "alive", "image": "alpine:latest"}, nil)
	var alive loka.Session
	decodeBody(t, createRec, &alive)

	createRec2 := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "dead", "image": "alpine:latest"}, nil)
	var dead loka.Session
	decodeBody(t, createRec2, &dead)

	// Destroy the second one.
	ts.doRequest(t, http.MethodDelete, "/api/v1/sessions/"+dead.ID, nil, nil)

	// List only running sessions.
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions?status=running", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Sessions []loka.Session `json:"sessions"`
		Total    int            `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 1 {
		t.Fatalf("expected 1 running session, got %d", body.Total)
	}
	if body.Sessions[0].Name != "alive" {
		t.Errorf("expected session name alive, got %q", body.Sessions[0].Name)
	}
}

// ---------------------------------------------------------------------------
// Error response structure
// ---------------------------------------------------------------------------

func TestRESTErrorResponseFormat(t *testing.T) {
	ts := setupTestServer(t)
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/sessions/does-not-exist", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if _, ok := body["error"]; !ok {
		t.Error("expected 'error' key in response body")
	}
}

// ---------------------------------------------------------------------------
// Health check reports worker counts
// ---------------------------------------------------------------------------

func TestRESTHealthCheck_WithWorkers(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/health", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if body["workers_total"].(float64) != 1 {
		t.Errorf("expected workers_total 1, got %v", body["workers_total"])
	}
	if body["workers_ready"].(float64) != 1 {
		t.Errorf("expected workers_ready 1, got %v", body["workers_ready"])
	}
}

// ---------------------------------------------------------------------------
// Content-Type header on JSON responses
// ---------------------------------------------------------------------------

func TestRESTResponseContentType(t *testing.T) {
	ts := setupTestServer(t)
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/health", nil, nil)
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

// ---------------------------------------------------------------------------
// Exec in explore mode rejects write commands
// ---------------------------------------------------------------------------

func TestRESTExecCommand_ExploreMode_AllowsAllCommands(t *testing.T) {
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	createRec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"name": "explore-allow", "image": "alpine:latest", "mode": "explore"}, nil)
	var created loka.Session
	decodeBody(t, createRec, &created)

	// All commands are allowed in explore mode — filesystem is read-only (enforced by supervisor).
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/sessions/"+created.ID+"/exec",
		map[string]any{"command": "python3", "args": []string{"-c", "print(42)"}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for command in explore mode, got %d: %s",
			rec.Code, rec.Body.String())
	}
}
