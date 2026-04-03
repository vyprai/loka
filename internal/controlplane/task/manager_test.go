package task

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	cpworker "github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
	"github.com/vyprai/loka/internal/store/sqlite"

	_ "modernc.org/sqlite"
)

// setupTestStore creates an in-memory SQLite store for testing.
func setupTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// createTestTask inserts a task with the given status and creation time.
func createTestTask(t *testing.T, s *sqlite.Store, name string, status loka.TaskStatus, createdAt time.Time) *loka.Task {
	t.Helper()
	task := &loka.Task{
		ID:        uuid.New().String(),
		Name:      name,
		Status:    status,
		ImageRef:  "alpine:latest",
		Command:   "echo",
		Args:      []string{"hello"},
		VCPUs:     1,
		MemoryMB:  256,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := s.Tasks().Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

// newManagerFromStore creates a task Manager, triggering recoverStuckTasks.
func newManagerFromStore(t *testing.T, s *sqlite.Store) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := cpworker.NewRegistry(s, logger, nil)
	sched := scheduler.New(reg, "", nil)

	dataDir := t.TempDir()
	objStore, err := local.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	imgMgr := image.NewManager(objStore, dataDir, logger)

	// NewManager calls recoverStuckTasks in its constructor.
	return NewManager(s, reg, sched, imgMgr, logger)
}

func TestRecoverStuckTasks_MarksOldRunningAsError(t *testing.T) {
	s := setupTestStore(t)

	// Insert a task stuck in "running" from 20 minutes ago.
	oldTime := time.Now().Add(-20 * time.Minute)
	task := createTestTask(t, s, "stuck-running", loka.TaskStatusRunning, oldTime)

	// Creating the manager triggers recoverStuckTasks.
	_ = newManagerFromStore(t, s)

	got, err := s.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.TaskStatusError {
		t.Errorf("status = %q, want %q", got.Status, loka.TaskStatusError)
	}
	if got.StatusMessage != "interrupted by restart" {
		t.Errorf("status message = %q, want %q", got.StatusMessage, "interrupted by restart")
	}
	if got.CompletedAt.IsZero() {
		t.Error("expected CompletedAt to be set")
	}
}

func TestRecoverStuckTasks_MarksOldPendingAsError(t *testing.T) {
	s := setupTestStore(t)

	// Insert a task stuck in "pending" from 20 minutes ago.
	oldTime := time.Now().Add(-20 * time.Minute)
	task := createTestTask(t, s, "stuck-pending", loka.TaskStatusPending, oldTime)

	_ = newManagerFromStore(t, s)

	got, err := s.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.TaskStatusError {
		t.Errorf("status = %q, want %q", got.Status, loka.TaskStatusError)
	}
	if got.StatusMessage != "interrupted by restart" {
		t.Errorf("status message = %q, want %q", got.StatusMessage, "interrupted by restart")
	}
}

func TestRecoverStuckTasks_LeavesRecentAlone(t *testing.T) {
	s := setupTestStore(t)

	// Insert a running task from 2 minutes ago (within the 10-min threshold).
	recentTime := time.Now().Add(-2 * time.Minute)
	task := createTestTask(t, s, "recent-running", loka.TaskStatusRunning, recentTime)

	_ = newManagerFromStore(t, s)

	got, err := s.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.TaskStatusRunning {
		t.Errorf("status = %q, want %q (should not be changed)", got.Status, loka.TaskStatusRunning)
	}
}

func TestRecoverStuckTasks_LeavesCompletedAlone(t *testing.T) {
	s := setupTestStore(t)

	// Insert a "success" task from 20 minutes ago.
	oldTime := time.Now().Add(-20 * time.Minute)
	task := createTestTask(t, s, "completed-task", loka.TaskStatusSuccess, oldTime)

	_ = newManagerFromStore(t, s)

	got, err := s.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.TaskStatusSuccess {
		t.Errorf("status = %q, want %q (completed tasks should not be changed)", got.Status, loka.TaskStatusSuccess)
	}
}

func TestRecoverStuckTasks_LeavesFailedAlone(t *testing.T) {
	s := setupTestStore(t)

	// Insert a "failed" task from 20 minutes ago.
	oldTime := time.Now().Add(-20 * time.Minute)
	task := createTestTask(t, s, "failed-task", loka.TaskStatusFailed, oldTime)

	_ = newManagerFromStore(t, s)

	got, err := s.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.TaskStatusFailed {
		t.Errorf("status = %q, want %q (failed tasks should not be changed)", got.Status, loka.TaskStatusFailed)
	}
}
