package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

func setupVolTestDB(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestVolumeCreate_AllFields(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	now := time.Now()
	vol := &loka.VolumeRecord{
		Name:             "testvol",
		Type:             loka.VolumeTypeBlock,
		Status:           loka.VolumeStatusHealthy,
		Provider:         "volume",
		SizeBytes:        1024,
		MaxSizeBytes:     1048576,
		PrimaryWorkerID:  "w1",
		ReplicaWorkerIDs: []string{"w2", "w3"},
		DesiredReplicas:  2,
		MountCount:       1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := s.Volumes().Create(ctx, vol); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Volumes().Get(ctx, "testvol")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Type != loka.VolumeTypeBlock {
		t.Errorf("Type: got %s, want block", got.Type)
	}
	if got.Status != loka.VolumeStatusHealthy {
		t.Errorf("Status: got %s, want healthy", got.Status)
	}
	if got.SizeBytes != 1024 {
		t.Errorf("SizeBytes: got %d, want 1024", got.SizeBytes)
	}
	if got.MaxSizeBytes != 1048576 {
		t.Errorf("MaxSizeBytes: got %d, want 1048576", got.MaxSizeBytes)
	}
	if got.PrimaryWorkerID != "w1" {
		t.Errorf("PrimaryWorkerID: got %s, want w1", got.PrimaryWorkerID)
	}
	if len(got.ReplicaWorkerIDs) != 2 || got.ReplicaWorkerIDs[0] != "w2" {
		t.Errorf("ReplicaWorkerIDs: got %v, want [w2 w3]", got.ReplicaWorkerIDs)
	}
}

func TestVolumeCreate_NilReplicas(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name:      "vol-nil",
		Type:      loka.VolumeTypeBlock,
		Status:    loka.VolumeStatusDegraded,
		Provider:  "volume",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	got, _ := s.Volumes().Get(ctx, "vol-nil")
	if got.ReplicaWorkerIDs == nil {
		// nil is acceptable — but should not panic.
	}
}

func TestVolumeUpdate_Roundtrip(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	now := time.Now()
	vol := &loka.VolumeRecord{
		Name:      "upd",
		Type:      loka.VolumeTypeObject,
		Status:    loka.VolumeStatusHealthy,
		Provider:  "s3",
		Bucket:    "my-bucket",
		Prefix:    "pfx/",
		Region:    "us-east-1",
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.Volumes().Create(ctx, vol)

	vol.SizeBytes = 999
	vol.MaxSizeBytes = 10000
	vol.Bucket = "new-bucket"
	vol.UpdatedAt = time.Now()
	s.Volumes().Update(ctx, vol)

	got, _ := s.Volumes().Get(ctx, "upd")
	if got.SizeBytes != 999 {
		t.Errorf("SizeBytes not updated: %d", got.SizeBytes)
	}
	if got.Bucket != "new-bucket" {
		t.Errorf("Bucket not updated: %s", got.Bucket)
	}
}

func TestVolumeUpdatePlacement(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name: "place", Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusDegraded,
		Provider: "volume", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	s.Volumes().UpdatePlacement(ctx, "place", "w1", []string{"w2", "w3"})

	got, _ := s.Volumes().Get(ctx, "place")
	if got.PrimaryWorkerID != "w1" {
		t.Errorf("PrimaryWorkerID: got %s", got.PrimaryWorkerID)
	}
	if len(got.ReplicaWorkerIDs) != 2 {
		t.Errorf("ReplicaWorkerIDs: got %v", got.ReplicaWorkerIDs)
	}
}

func TestVolumeUpdateStatus(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name: "stat", Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusHealthy,
		Provider: "volume", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	s.Volumes().UpdateStatus(ctx, "stat", loka.VolumeStatusDegraded)
	got, _ := s.Volumes().Get(ctx, "stat")
	if got.Status != loka.VolumeStatusDegraded {
		t.Errorf("Status: got %s", got.Status)
	}
}

func TestVolumeListByWorker(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	for _, name := range []string{"v1", "v2", "v3"} {
		vol := &loka.VolumeRecord{
			Name: name, Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusHealthy,
			Provider: "volume", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		s.Volumes().Create(ctx, vol)
	}

	// v1 primary=w1, v2 primary=w2 replica=w1, v3 primary=w3
	s.Volumes().UpdatePlacement(ctx, "v1", "w1", nil)
	s.Volumes().UpdatePlacement(ctx, "v2", "w2", []string{"w1"})
	s.Volumes().UpdatePlacement(ctx, "v3", "w3", nil)

	vols, _ := s.Volumes().ListByWorker(ctx, "w1")
	names := map[string]bool{}
	for _, v := range vols {
		names[v.Name] = true
	}
	if !names["v1"] || !names["v2"] {
		t.Errorf("expected v1 and v2, got %v", names)
	}
	if names["v3"] {
		t.Error("v3 should not be returned for w1")
	}
}

func TestAtomicIncrementMountCount(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name: "atomic", Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusHealthy,
		Provider: "volume", MountCount: 0, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	// Concurrent increments.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Volumes().IncrementMountCount(ctx, "atomic")
		}()
	}
	wg.Wait()

	got, _ := s.Volumes().Get(ctx, "atomic")
	if got.MountCount != 20 {
		t.Errorf("expected mount count 20, got %d", got.MountCount)
	}
}

func TestAtomicDecrementMountCount(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name: "decr", Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusHealthy,
		Provider: "volume", MountCount: 10, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Volumes().DecrementMountCount(ctx, "decr")
		}()
	}
	wg.Wait()

	got, _ := s.Volumes().Get(ctx, "decr")
	if got.MountCount != 0 {
		t.Errorf("expected 0, got %d", got.MountCount)
	}
}

func TestDecrementMountCount_ClampsAtZero(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name: "clamp", Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusHealthy,
		Provider: "volume", MountCount: 0, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	// Decrement below zero — should clamp.
	s.Volumes().DecrementMountCount(ctx, "clamp")
	s.Volumes().DecrementMountCount(ctx, "clamp")

	got, _ := s.Volumes().Get(ctx, "clamp")
	if got.MountCount != 0 {
		t.Errorf("expected 0 (clamped), got %d", got.MountCount)
	}
}

func TestConcurrentIncrementDecrement(t *testing.T) {
	s := setupVolTestDB(t)
	ctx := context.Background()

	vol := &loka.VolumeRecord{
		Name: "mixed", Type: loka.VolumeTypeBlock, Status: loka.VolumeStatusHealthy,
		Provider: "volume", MountCount: 0, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Volumes().Create(ctx, vol)

	// 30 increments + 10 decrements = 20 final count.
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Volumes().IncrementMountCount(ctx, "mixed")
		}()
	}
	wg.Wait()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Volumes().DecrementMountCount(ctx, "mixed")
		}()
	}
	wg.Wait()

	got, _ := s.Volumes().Get(ctx, "mixed")
	if got.MountCount != 20 {
		t.Errorf("expected 20, got %d", got.MountCount)
	}
}

func TestVolumeDelete_NotFound(t *testing.T) {
	s := setupVolTestDB(t)
	err := s.Volumes().Delete(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error deleting non-existent volume")
	}
}

func TestVolumeList_Empty(t *testing.T) {
	s := setupVolTestDB(t)
	vols, err := s.Volumes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 0 {
		t.Errorf("expected empty list, got %d", len(vols))
	}
}
