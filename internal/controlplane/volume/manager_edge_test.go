package volume

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store/sqlite"
)

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Input Validation & Partial Failures
// ═══════════════════════════════════════════════════════════════

func TestCreateBlock_EmptyName(t *testing.T) {
	m := setupTestManager(t, nil)
	_, err := m.CreateBlock(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestCreateObject_EmptyName(t *testing.T) {
	m := setupTestManager(t, nil)
	_, err := m.CreateObject(context.Background(), "", "bucket", "", "", "", 0)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestAssignPrimary_NonExistentVolume(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	err := m.AssignPrimary(context.Background(), "ghost", "w1")
	if err == nil {
		t.Fatal("expected error for non-existent volume")
	}
}

func TestAssignReplica_NonExistentVolume(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	err := m.AssignReplica(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for non-existent volume")
	}
}

func TestAssignPrimary_DirectObjectSkipped(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	ctx := context.Background()

	m.CreateObject(ctx, "direct", "my-bucket", "pfx/", "us-east-1", "", 0)
	if err := m.AssignPrimary(ctx, "direct", "w1"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	vol, _ := m.Get(ctx, "direct")
	if vol.PrimaryWorkerID != "" {
		t.Errorf("direct object should not have primary, got %s", vol.PrimaryWorkerID)
	}
}

func TestAssignReplica_DirectObjectSkipped(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()

	m.CreateObject(ctx, "direct", "bucket", "", "", "", 0)
	if err := m.AssignReplica(ctx, "direct"); err != nil {
		t.Fatalf("expected no error for direct object, got %v", err)
	}
}

func TestAssignReplica_WorkerRegistryError(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")

	m.workers.(*mockWorkerRegistry).setError(fmt.Errorf("network error"))
	err := m.AssignReplica(ctx, "vol1")
	if err == nil {
		t.Fatal("expected error when worker registry fails")
	}
}

func TestAssignReplica_NilWorkerRegistry(t *testing.T) {
	s, _ := sqlite.New(":memory:")
	s.Migrate(context.Background())
	t.Cleanup(func() { s.Close() })

	mgr := NewManager(s, nil, nil, slog.Default(), nil)
	ctx := context.Background()
	mgr.CreateBlock(ctx, "vol1", 0)
	mgr.AssignPrimary(ctx, "vol1", "w1")

	if err := mgr.AssignReplica(ctx, "vol1"); err == nil {
		t.Fatal("expected error with nil worker registry")
	}
}

func TestAssignReplica_DuplicateDoesNotAddTwice(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1") // assigns w2
	m.AssignReplica(ctx, "vol1") // no more workers — should not panic

	vol, _ := m.Get(ctx, "vol1")
	if len(vol.ReplicaWorkerIDs) != 1 {
		t.Errorf("expected 1 replica, got %d: %v", len(vol.ReplicaWorkerIDs), vol.ReplicaWorkerIDs)
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Worker Death — Data Loss & Cascading Failures
// ═══════════════════════════════════════════════════════════════

func TestHandleWorkerDeath_PrimaryDead_NoReplicas_DataLoss(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")

	m.workers.(*mockWorkerRegistry).setWorkers(nil)
	m.HandleWorkerDeath(ctx, "w1")

	vol, _ := m.Get(ctx, "vol1")
	if vol.Status != loka.VolumeStatusError {
		t.Errorf("expected error status (data loss), got %s", vol.Status)
	}
	if vol.PrimaryWorkerID != "" {
		t.Errorf("expected empty primary after data loss, got %s", vol.PrimaryWorkerID)
	}
}

func TestHandleWorkerDeath_NoVolumesOnWorker(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	if err := m.HandleWorkerDeath(context.Background(), "w2"); err != nil {
		t.Fatalf("should not error: %v", err)
	}
}

func TestHandleWorkerDeath_MultipleVolumes(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()

	for _, name := range []string{"vol-a", "vol-b", "vol-c"} {
		m.CreateBlock(ctx, name, 0)
		m.AssignPrimary(ctx, name, "w1")
		m.AssignReplica(ctx, name)
	}

	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w2", "w3"})
	m.HandleWorkerDeath(ctx, "w1")

	for _, name := range []string{"vol-a", "vol-b", "vol-c"} {
		vol, _ := m.Get(ctx, name)
		if vol.PrimaryWorkerID != "w2" {
			t.Errorf("%s: expected primary w2, got %s", name, vol.PrimaryWorkerID)
		}
	}
}

func TestHandleWorkerDeath_WorkerIsBothPrimaryAndReplica(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()

	// vol1: primary=w1, replica=w2
	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1")

	// vol2: primary=w2, replica=w1 (manually)
	m.CreateBlock(ctx, "vol2", 0)
	m.AssignPrimary(ctx, "vol2", "w2")
	vol2, _ := m.Get(ctx, "vol2")
	vol2.ReplicaWorkerIDs = []string{"w1"}
	vol2.Status = loka.VolumeStatusHealthy
	m.store.Volumes().Update(ctx, vol2)

	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w2", "w3"})
	m.HandleWorkerDeath(ctx, "w1")

	v1, _ := m.Get(ctx, "vol1")
	if v1.PrimaryWorkerID != "w2" {
		t.Errorf("vol1: expected w2 promoted, got %s", v1.PrimaryWorkerID)
	}

	v2, _ := m.Get(ctx, "vol2")
	for _, id := range v2.ReplicaWorkerIDs {
		if id == "w1" {
			t.Error("vol2: dead worker w1 still in replicas")
		}
	}
}

func TestHandleWorkerDeath_SkipsDirectObjectVolumes(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()
	m.CreateObject(ctx, "direct", "bucket", "pfx/", "us-east-1", "", 0)
	if err := m.HandleWorkerDeath(ctx, "w1"); err != nil {
		t.Fatalf("should ignore direct object: %v", err)
	}
}

func TestAllWorkersDie_AllVolumesFail(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1")

	m.CreateBlock(ctx, "vol2", 0)
	m.AssignPrimary(ctx, "vol2", "w2")
	v2, _ := m.Get(ctx, "vol2")
	v2.ReplicaWorkerIDs = []string{"w1"}
	v2.Status = loka.VolumeStatusHealthy
	m.store.Volumes().Update(ctx, v2)

	// Kill w1 first.
	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w2"})
	m.HandleWorkerDeath(ctx, "w1")

	v1, _ := m.Get(ctx, "vol1")
	if v1.PrimaryWorkerID != "w2" {
		t.Errorf("vol1 should be promoted to w2, got %s", v1.PrimaryWorkerID)
	}

	// Now kill w2 — last worker.
	m.workers.(*mockWorkerRegistry).setWorkers(nil)
	m.HandleWorkerDeath(ctx, "w2")

	v1, _ = m.Get(ctx, "vol1")
	if v1.Status != loka.VolumeStatusError {
		t.Errorf("vol1 expected error, got %s", v1.Status)
	}
	v2After, _ := m.Get(ctx, "vol2")
	if v2After.Status != loka.VolumeStatusError {
		t.Errorf("vol2 expected error, got %s", v2After.Status)
	}
}

func TestRapidWorkerChurn(t *testing.T) {
	all := []string{"w1", "w2", "w3", "w4", "w5"}
	m := setupTestManager(t, all)
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1")

	dead := map[string]bool{}
	for _, d := range []string{"w1", "w2", "w3"} {
		dead[d] = true
		var remaining []string
		for _, w := range all {
			if !dead[w] {
				remaining = append(remaining, w)
			}
		}
		m.workers.(*mockWorkerRegistry).setWorkers(remaining)
		m.HandleWorkerDeath(ctx, d)
	}

	vol, _ := m.Get(ctx, "vol1")
	if vol.PrimaryWorkerID != "w4" && vol.PrimaryWorkerID != "w5" {
		t.Errorf("expected primary w4 or w5, got %s", vol.PrimaryWorkerID)
	}
	if vol.Status == loka.VolumeStatusError {
		t.Error("volume should not be error, w4/w5 still alive")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Mount Count Integrity
// ═══════════════════════════════════════════════════════════════

func TestIncrementMountCount_AutoCreatesVolume(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx := context.Background()

	m.IncrementMountCount(ctx, "auto-vol")
	vol, _ := m.Get(ctx, "auto-vol")
	if vol.MountCount != 1 || vol.Type != loka.VolumeTypeBlock {
		t.Errorf("auto-created: count=%d type=%s", vol.MountCount, vol.Type)
	}
}

func TestDecrementMountCount_ClampsAtZero(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.DecrementMountCount(ctx, "vol1")
	vol, _ := m.Get(ctx, "vol1")
	if vol.MountCount < 0 {
		t.Errorf("mount count should not go negative, got %d", vol.MountCount)
	}
}

func TestDecrementMountCount_NonExistent(t *testing.T) {
	m := setupTestManager(t, nil)
	if err := m.DecrementMountCount(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteVolume_MountedFails(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.IncrementMountCount(ctx, "vol1")
	if err := m.Delete(ctx, "vol1"); err == nil {
		t.Fatal("expected error deleting mounted volume")
	}
}

func TestDeleteVolume_NonExistent(t *testing.T) {
	m := setupTestManager(t, nil)
	if err := m.Delete(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Concurrent Operations
// ═══════════════════════════════════════════════════════════════

func TestConcurrentAssignPrimary(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)

	var wg sync.WaitGroup
	for _, w := range []string{"w1", "w2", "w3"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			m.AssignPrimary(ctx, "vol1", id)
		}(w)
	}
	wg.Wait()

	vol, _ := m.Get(ctx, "vol1")
	valid := vol.PrimaryWorkerID == "w1" || vol.PrimaryWorkerID == "w2" || vol.PrimaryWorkerID == "w3"
	if !valid {
		t.Errorf("primary should be a valid worker, got %s", vol.PrimaryWorkerID)
	}
}

func TestConcurrentHandleWorkerDeath(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1")

	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w3"})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.HandleWorkerDeath(ctx, "w1")
		}()
	}
	wg.Wait()

	vol, _ := m.Get(ctx, "vol1")
	if vol.PrimaryWorkerID == "w1" {
		t.Error("dead worker should not remain primary")
	}
}

func TestConcurrentMountCountUpdates(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.IncrementMountCount(ctx, "vol1")
		}()
	}
	wg.Wait()
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.DecrementMountCount(ctx, "vol1")
		}()
	}
	wg.Wait()

	vol, _ := m.Get(ctx, "vol1")
	if vol.MountCount != 5 {
		t.Errorf("expected 5, got %d", vol.MountCount)
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Reconciliation
// ═══════════════════════════════════════════════════════════════

func TestReconcile_IgnoresHealthyVolumes(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w1")
	m.AssignReplica(ctx, "vol1")

	m.workers.(*mockWorkerRegistry).setWorkers([]string{"w1", "w2", "w3"})
	m.ReconcileDegradedVolumes(ctx)

	vol, _ := m.Get(ctx, "vol1")
	if len(vol.ReplicaWorkerIDs) != 1 {
		t.Errorf("healthy vol should keep 1 replica, got %d", len(vol.ReplicaWorkerIDs))
	}
}

func TestReconcile_SkipsErrorVolumes(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.store.Volumes().UpdateStatus(ctx, "vol1", loka.VolumeStatusError)
	m.ReconcileDegradedVolumes(ctx)

	vol, _ := m.Get(ctx, "vol1")
	if len(vol.ReplicaWorkerIDs) != 0 {
		t.Error("error volume should not get replicas")
	}
}

func TestReconcile_NoPrimarySkipped(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2"})
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.ReconcileDegradedVolumes(ctx)

	vol, _ := m.Get(ctx, "vol1")
	if len(vol.ReplicaWorkerIDs) != 0 {
		t.Error("volume without primary should not get replicas")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Size & Context
// ═══════════════════════════════════════════════════════════════

func TestUpdateSizeReport_Works(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	m.UpdateSizeReport(ctx, "vol1", 1024*1024)
	vol, _ := m.Get(ctx, "vol1")
	if vol.SizeBytes != 1024*1024 {
		t.Errorf("expected 1MB, got %d", vol.SizeBytes)
	}
}

func TestUpdateSizeReport_NonExistent(t *testing.T) {
	m := setupTestManager(t, nil)
	if err := m.UpdateSizeReport(context.Background(), "ghost", 100); err == nil {
		t.Fatal("expected error")
	}
}

func TestListFiles_BlockVolumeReturnsNil(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx := context.Background()
	m.CreateBlock(ctx, "vol1", 0)
	files, _ := m.ListFiles(ctx, "vol1")
	if files != nil {
		t.Error("block volume should return nil files")
	}
}

func TestCancelledContext(t *testing.T) {
	m := setupTestManager(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.CreateBlock(ctx, "vol1", 0)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Store-Level ListByWorker
// ═══════════════════════════════════════════════════════════════

func TestListByWorker_FindsPrimaryAndReplica(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w2", "w3"})
	ctx := context.Background()

	// vol-a: primary=w1, replica=w2
	m.CreateBlock(ctx, "vol-a", 0)
	m.AssignPrimary(ctx, "vol-a", "w1")
	m.AssignReplica(ctx, "vol-a")

	// vol-b: primary=w3, replica=w1 (manually)
	m.CreateBlock(ctx, "vol-b", 0)
	m.AssignPrimary(ctx, "vol-b", "w3")
	vb, _ := m.Get(ctx, "vol-b")
	vb.ReplicaWorkerIDs = []string{"w1"}
	vb.Status = loka.VolumeStatusHealthy
	m.store.Volumes().Update(ctx, vb)

	vols, _ := m.store.Volumes().ListByWorker(ctx, "w1")
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes for w1, got %d", len(vols))
	}
	names := map[string]bool{}
	for _, v := range vols {
		names[v.Name] = true
	}
	if !names["vol-a"] || !names["vol-b"] {
		t.Errorf("expected vol-a and vol-b, got %v", names)
	}
}

func TestListByWorker_NoVolumes(t *testing.T) {
	m := setupTestManager(t, []string{"w1"})
	vols, _ := m.store.Volumes().ListByWorker(context.Background(), "w1")
	if len(vols) != 0 {
		t.Errorf("expected 0, got %d", len(vols))
	}
}

func TestListByWorker_DoesNotFalseMatchSubstring(t *testing.T) {
	m := setupTestManager(t, []string{"w1", "w10", "w2"})
	ctx := context.Background()

	m.CreateBlock(ctx, "vol1", 0)
	m.AssignPrimary(ctx, "vol1", "w10")

	// w1 should NOT match w10's primary.
	vols, _ := m.store.Volumes().ListByWorker(ctx, "w1")
	for _, v := range vols {
		if v.PrimaryWorkerID == "w10" {
			t.Error("w1 should not match w10 via substring")
		}
	}
}
