package volume

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store/sqlite"
)

type mockWorkerRegistry struct {
	mu      sync.Mutex
	workers []string
	err     error // if set, ListHealthy returns this error
}

func (m *mockWorkerRegistry) ListHealthy(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.workers, nil
}

func (m *mockWorkerRegistry) setWorkers(w []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers = w
}

func (m *mockWorkerRegistry) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func setupTestManager(t *testing.T, workers []string) *Manager {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	reg := &mockWorkerRegistry{workers: workers}
	return NewManager(s, nil, reg, slog.Default(), nil)
}

func TestCreateBlock(t *testing.T) {
	m := setupTestManager(t, nil)

	vol, err := m.CreateBlock(context.Background(), "mydata", 1024*1024)
	if err != nil {
		t.Fatalf("CreateBlock: %v", err)
	}
	if vol.Type != loka.VolumeTypeBlock {
		t.Errorf("expected block, got %s", vol.Type)
	}
	if vol.MaxSizeBytes != 1024*1024 {
		t.Errorf("expected max size 1MB, got %d", vol.MaxSizeBytes)
	}
	if vol.Status != loka.VolumeStatusDegraded {
		t.Errorf("expected degraded (no workers), got %s", vol.Status)
	}
	if vol.DesiredReplicas != 2 {
		t.Errorf("expected 2 desired replicas, got %d", vol.DesiredReplicas)
	}
}

func TestCreateBlock_Duplicate(t *testing.T) {
	m := setupTestManager(t, nil)
	m.CreateBlock(context.Background(), "dup", 0)
	_, err := m.CreateBlock(context.Background(), "dup", 0)
	if err == nil {
		t.Fatal("expected error for duplicate volume")
	}
}

func TestCreateObject_Direct(t *testing.T) {
	m := setupTestManager(t, nil)

	vol, err := m.CreateObject(context.Background(), "assets", "my-bucket", "prefix/", "us-east-1", "", 0)
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	if vol.Type != loka.VolumeTypeObject {
		t.Errorf("expected object, got %s", vol.Type)
	}
	if vol.Bucket != "my-bucket" {
		t.Errorf("expected my-bucket, got %s", vol.Bucket)
	}
	if !vol.IsDirectObject() {
		t.Error("expected direct object")
	}
	if vol.Status != loka.VolumeStatusHealthy {
		t.Errorf("expected healthy, got %s", vol.Status)
	}
}

func TestCreateObject_NoObjstore_FallsBackToBlock(t *testing.T) {
	m := setupTestManager(t, nil) // objStore is nil

	vol, err := m.CreateObject(context.Background(), "loka-managed", "", "", "", "", 0)
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	// No objstore → falls back to block.
	if vol.Type != loka.VolumeTypeBlock {
		t.Errorf("expected block fallback, got %s", vol.Type)
	}
	if vol.IsDirectObject() {
		t.Error("should not be direct object")
	}
	if !vol.IsLokaManaged() {
		t.Error("should be loka-managed")
	}
}

func TestAssignPrimary(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	if err := m.AssignPrimary(ctx, "vol1", "w1"); err != nil {
		t.Fatalf("AssignPrimary: %v", err)
	}

	vol, _ := m.Get(ctx, "vol1")
	if vol.PrimaryWorkerID != "w1" {
		t.Errorf("expected primary w1, got %s", vol.PrimaryWorkerID)
	}
}

func TestAssignReplica(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")

	if err := m.AssignReplica(ctx, "vol1"); err != nil {
		t.Fatalf("AssignReplica: %v", err)
	}

	vol, _ := m.Get(ctx, "vol1")
	if len(vol.ReplicaWorkerIDs) != 1 {
		t.Fatalf("expected 1 replica, got %d", len(vol.ReplicaWorkerIDs))
	}
	if vol.ReplicaWorkerIDs[0] != "w2" {
		t.Errorf("expected replica w2, got %s", vol.ReplicaWorkerIDs[0])
	}
}

func TestAssignReplica_SingleWorker_Degraded(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")

	// Only 1 worker — replica assignment should mark degraded.
	m.AssignReplica(ctx, "vol1")

	vol, _ := m.Get(ctx, "vol1")
	if vol.Status != loka.VolumeStatusDegraded {
		t.Errorf("expected degraded, got %s", vol.Status)
	}
	if len(vol.ReplicaWorkerIDs) != 0 {
		t.Errorf("expected no replicas, got %d", len(vol.ReplicaWorkerIDs))
	}
}

func TestHandleWorkerDeath_PrimaryDead(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1")

	// Kill w1.
	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w2", "w3"})
	if err := m.HandleWorkerDeath(ctx, "w1"); err != nil {
		t.Fatalf("HandleWorkerDeath: %v", err)
	}

	vol, _ := m.Get(ctx, "vol1")
	if vol.PrimaryWorkerID != "w2" {
		t.Errorf("expected w2 promoted to primary, got %s", vol.PrimaryWorkerID)
	}
}

func TestHandleWorkerDeath_ReplicaDead(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1") // w2

	// Kill w2 (replica).
	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w1", "w3"})
	if err := m.HandleWorkerDeath(ctx, "w2"); err != nil {
		t.Fatalf("HandleWorkerDeath: %v", err)
	}

	vol, _ := m.Get(ctx, "vol1")
	if vol.PrimaryWorkerID != "w1" {
		t.Errorf("primary should remain w1, got %s", vol.PrimaryWorkerID)
	}
	// w3 should be the new replica.
	if len(vol.ReplicaWorkerIDs) != 1 || vol.ReplicaWorkerIDs[0] != "w3" {
		t.Errorf("expected replica w3, got %v", vol.ReplicaWorkerIDs)
	}
}

func TestReconcileDegradedVolumes(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	// Only 1 worker → degraded.

	// Add a second worker.
	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w1", "w2"})
	if err := m.ReconcileDegradedVolumes(ctx); err != nil {
		t.Fatalf("ReconcileDegradedVolumes: %v", err)
	}

	vol, _ := m.Get(ctx, "vol1")
	if len(vol.ReplicaWorkerIDs) != 1 {
		t.Fatalf("expected 1 replica after reconcile, got %d", len(vol.ReplicaWorkerIDs))
	}
	if vol.ReplicaWorkerIDs[0] != "w2" {
		t.Errorf("expected replica w2, got %s", vol.ReplicaWorkerIDs[0])
	}
}
