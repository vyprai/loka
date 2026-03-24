package replicated

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/internal/store/sqlite"
)

// mockCoordinator implements the Coordinator interface for testing.
// It calls handlers directly (like LocalCoordinator).
type mockCoordinator struct {
	handlers map[string]func([]byte) interface{}
}

func newMockCoordinator() *mockCoordinator {
	return &mockCoordinator{handlers: make(map[string]func([]byte) interface{})}
}

func (m *mockCoordinator) Apply(_ context.Context, cmd []byte) (interface{}, error) {
	var envelope struct {
		Op string `json:"op"`
	}
	json.Unmarshal(cmd, &envelope)
	if fn, ok := m.handlers[envelope.Op]; ok {
		return fn(cmd), nil
	}
	return nil, nil
}

func (m *mockCoordinator) RegisterHandler(op string, fn func([]byte) interface{}) {
	m.handlers[op] = fn
}

func (m *mockCoordinator) IsLeader(_ string) bool { return true }

func setupReplicatedStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("create sqlite: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	coord := newMockCoordinator()
	return New(db, coord, logger)
}

func TestReplicatedStore_SessionCRUD(t *testing.T) {
	s := setupReplicatedStore(t)
	ctx := context.Background()

	session := &loka.Session{
		ID:        "sess-1",
		Name:      "test-session",
		Status:    loka.SessionStatusCreating,
		Mode:      loka.ModeExecute,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Create.
	if err := s.Sessions().Create(ctx, session); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Get.
	got, err := s.Sessions().Get(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "test-session" {
		t.Errorf("Name = %q, want %q", got.Name, "test-session")
	}

	// Update.
	session.Status = loka.SessionStatusRunning
	if err := s.Sessions().Update(ctx, session); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = s.Sessions().Get(ctx, "sess-1")
	if got.Status != loka.SessionStatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, loka.SessionStatusRunning)
	}

	// List.
	list, err := s.Sessions().List(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List length = %d, want 1", len(list))
	}

	// Delete.
	if err := s.Sessions().Delete(ctx, "sess-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Sessions().Get(ctx, "sess-1")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestReplicatedStore_WorkerCRUD(t *testing.T) {
	s := setupReplicatedStore(t)
	ctx := context.Background()

	w := &loka.Worker{
		ID:        "worker-1",
		Hostname:  "test-host",
		Status:    loka.WorkerStatusReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.Workers().Create(ctx, w); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Workers().Get(ctx, "worker-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hostname != "test-host" {
		t.Errorf("Hostname = %q, want %q", got.Hostname, "test-host")
	}

	// UpdateHeartbeat.
	hb := &loka.Heartbeat{
		WorkerID:  "worker-1",
		Timestamp: time.Now(),
		Status:    loka.WorkerStatusReady,
	}
	if err := s.Workers().UpdateHeartbeat(ctx, "worker-1", hb); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
}

func TestReplicatedStore_ExecutionCRUD(t *testing.T) {
	s := setupReplicatedStore(t)
	ctx := context.Background()

	// Create a session first (foreign key).
	s.Sessions().Create(ctx, &loka.Session{
		ID: "sess-exec", Name: "exec-test", Status: loka.SessionStatusRunning,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	exec := &loka.Execution{
		ID:        "exec-1",
		SessionID: "sess-exec",
		Status:    loka.ExecStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.Executions().Create(ctx, exec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Executions().Get(ctx, "exec-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SessionID != "sess-exec" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-exec")
	}
}

func TestReplicatedStore_CheckpointCRUD(t *testing.T) {
	s := setupReplicatedStore(t)
	ctx := context.Background()

	s.Sessions().Create(ctx, &loka.Session{
		ID: "sess-cp", Name: "cp-test", Status: loka.SessionStatusRunning,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	cp := &loka.Checkpoint{
		ID:        "cp-1",
		SessionID: "sess-cp",
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusCreating,
		CreatedAt: time.Now(),
	}

	if err := s.Checkpoints().Create(ctx, cp); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Checkpoints().Get(ctx, "cp-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SessionID != "sess-cp" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-cp")
	}

	if err := s.Checkpoints().Delete(ctx, "cp-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestReplicatedStore_TokenCRUD(t *testing.T) {
	s := setupReplicatedStore(t)
	ctx := context.Background()

	token := &loka.WorkerToken{
		ID:        "tok-1",
		Name:      "test-token",
		Token:     "loka_test123",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now(),
	}

	if err := s.Tokens().Create(ctx, token); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Tokens().Get(ctx, "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "test-token" {
		t.Errorf("Name = %q, want %q", got.Name, "test-token")
	}

	if err := s.Tokens().MarkUsed(ctx, "tok-1", "worker-x"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	if err := s.Tokens().Delete(ctx, "tok-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestReplicatedStore_TwoNodes simulates two nodes sharing the same Raft log.
func TestReplicatedStore_TwoNodes(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Simulate: one coordinator, two local SQLite stores.
	coord := newMockCoordinator()

	db1, _ := sqlite.New(":memory:")
	db1.Migrate(ctx)
	t.Cleanup(func() { db1.Close() })

	db2, _ := sqlite.New(":memory:")
	db2.Migrate(ctx)
	t.Cleanup(func() { db2.Close() })

	// Both stores register handlers on the same coordinator.
	// In real Raft, Apply is called on ALL nodes' FSMs.
	store1 := New(db1, coord, logger)

	// Manually register a second handler to simulate node 2.
	// Override the coordinator to dispatch to both.
	origHandler := coord.handlers[opName]
	coord.handlers[opName] = func(data []byte) interface{} {
		// Apply on node 1 (via the registered handler).
		origHandler(data)
		// Also apply on node 2 directly.
		var cmd storeCmd
		json.Unmarshal(data, &cmd)
		store2ctx := context.Background()
		switch cmd.Entity {
		case "session":
			if cmd.Action == "create" {
				var s loka.Session
				json.Unmarshal(cmd.Data, &s)
				db2.Sessions().Create(store2ctx, &s)
			}
		}
		return nil
	}

	// Write on node 1 (through consensus).
	session := &loka.Session{
		ID: "shared-1", Name: "shared-session", Status: loka.SessionStatusRunning,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store1.Sessions().Create(ctx, session); err != nil {
		t.Fatalf("Create on node1: %v", err)
	}

	// Read on node 1 — should exist.
	got1, err := db1.Sessions().Get(ctx, "shared-1")
	if err != nil {
		t.Fatalf("Get on node1: %v", err)
	}
	if got1.Name != "shared-session" {
		t.Errorf("node1 Name = %q, want %q", got1.Name, "shared-session")
	}

	// Read on node 2 — should also exist (replicated).
	got2, err := db2.Sessions().Get(ctx, "shared-1")
	if err != nil {
		t.Fatalf("Get on node2: %v (replication failed)", err)
	}
	if got2.Name != "shared-session" {
		t.Errorf("node2 Name = %q, want %q", got2.Name, "shared-session")
	}
}
