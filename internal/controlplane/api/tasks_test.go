package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/controlplane/task"
	"github.com/vyprai/loka/internal/loka"
)

// setupTaskTestServer creates a test server with a real task.Manager wired up.
func setupTaskTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	taskMgr := task.NewManager(ts.store, ts.registry, ts.sched, ts.imgMgr, ts.server.logger)
	ts.server.taskManager = taskMgr

	// Wire up service manager so getTaskLogs (which delegates to getServiceLogs) doesn't panic.
	svcMgr := service.NewManager(ts.store, ts.registry, ts.sched, ts.imgMgr, nil, nil, ts.server.logger, nil)
	t.Cleanup(func() { svcMgr.Close() })
	ts.server.serviceManager = svcMgr

	return ts
}

// createTestTask inserts a task directly into the store for deterministic tests.
func createTestTask(t *testing.T, ts *testServer, name string, status loka.TaskStatus) *loka.Task {
	t.Helper()
	now := time.Now()
	tsk := &loka.Task{
		ID:        uuid.New().String(),
		Name:      name,
		Status:    status,
		ImageRef:  "alpine:latest",
		Command:   "echo",
		Args:      []string{"hello"},
		VCPUs:     1,
		MemoryMB:  256,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := ts.store.Tasks().Create(context.Background(), tsk); err != nil {
		t.Fatalf("create test task: %v", err)
	}
	return tsk
}

func TestRunTask(t *testing.T) {
	ts := setupTaskTestServer(t)

	payload := map[string]any{
		"name":    "my-task",
		"image":   "alpine:latest",
		"command": "echo hello",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/tasks", payload, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var tsk loka.Task
	decodeBody(t, rec, &tsk)
	if tsk.Name != "my-task" {
		t.Errorf("expected name my-task, got %q", tsk.Name)
	}
	if tsk.ID == "" {
		t.Error("expected non-empty task ID")
	}
	if tsk.ImageRef != "alpine:latest" {
		t.Errorf("expected image alpine:latest, got %q", tsk.ImageRef)
	}
}

func TestRunTask_MissingImage(t *testing.T) {
	ts := setupTaskTestServer(t)

	payload := map[string]any{
		"name":    "bad-task",
		"command": "echo hello",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/tasks", payload, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunTask_MissingCommand(t *testing.T) {
	ts := setupTaskTestServer(t)

	payload := map[string]any{
		"name":  "bad-task",
		"image": "alpine:latest",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/tasks", payload, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListTasks(t *testing.T) {
	ts := setupTaskTestServer(t)
	createTestTask(t, ts, "task-a", loka.TaskStatusSuccess)
	createTestTask(t, ts, "task-b", loka.TaskStatusFailed)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/tasks", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Tasks []loka.Task `json:"tasks"`
		Total int         `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total < 2 {
		t.Errorf("expected at least 2 tasks, got %d", body.Total)
	}
}

func TestGetTask(t *testing.T) {
	ts := setupTaskTestServer(t)
	created := createTestTask(t, ts, "get-me", loka.TaskStatusSuccess)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/tasks/"+created.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var tsk loka.Task
	decodeBody(t, rec, &tsk)
	if tsk.ID != created.ID {
		t.Errorf("expected ID %s, got %s", created.ID, tsk.ID)
	}
	if tsk.Name != "get-me" {
		t.Errorf("expected name get-me, got %q", tsk.Name)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	ts := setupTaskTestServer(t)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/tasks/nonexistent-id", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCancelTask(t *testing.T) {
	ts := setupTaskTestServer(t)
	created := createTestTask(t, ts, "cancel-me", loka.TaskStatusRunning)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	decodeBody(t, rec, &body)
	if body["status"] != "cancelled" {
		t.Errorf("expected status cancelled, got %q", body["status"])
	}

	// Verify task is now failed/cancelled in store.
	got, err := ts.store.Tasks().Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != loka.TaskStatusFailed {
		t.Errorf("expected status failed after cancel, got %q", got.Status)
	}
}

func TestDeleteTask(t *testing.T) {
	ts := setupTaskTestServer(t)
	created := createTestTask(t, ts, "delete-me", loka.TaskStatusSuccess)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/tasks/"+created.ID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify task is gone.
	_, err := ts.store.Tasks().Get(context.Background(), created.ID)
	if err == nil {
		t.Error("expected error fetching deleted task, got nil")
	}
}

func TestGetTaskLogs(t *testing.T) {
	ts := setupTaskTestServer(t)
	created := createTestTask(t, ts, "logs-task", loka.TaskStatusRunning)

	// Task logs delegate to service logs, which requires a running service VM.
	// Without a real worker, we expect an error response.
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/tasks/"+created.ID+"/logs", nil, nil)
	// Should return an error (service not found or no log fn).
	if rec.Code == http.StatusOK {
		t.Fatal("expected error for task logs without a running VM, got 200")
	}
}
