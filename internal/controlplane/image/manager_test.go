package image

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	tmpDir := t.TempDir()
	objStore, err := local.New(tmpDir)
	if err != nil {
		t.Fatalf("create local objstore: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewManager(objStore, tmpDir, logger)
}

func TestNewManager(t *testing.T) {
	m := newTestManager(t)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if m.images == nil {
		t.Fatal("expected images map to be initialized")
	}
	if m.objStore == nil {
		t.Fatal("expected objStore to be set")
	}
	if m.logger == nil {
		t.Fatal("expected logger to be set")
	}
}

func TestManager_List_Empty(t *testing.T) {
	m := newTestManager(t)
	imgs := m.List()
	if len(imgs) != 0 {
		t.Errorf("expected 0 images, got %d", len(imgs))
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	m := newTestManager(t)
	_, ok := m.Get("nonexistent")
	if ok {
		t.Error("expected Get to return false for nonexistent image")
	}
}

func TestManager_GetByRef_NotFound(t *testing.T) {
	m := newTestManager(t)
	_, ok := m.GetByRef("ubuntu:22.04")
	if ok {
		t.Error("expected GetByRef to return false for nonexistent reference")
	}
}

func TestManager_Delete_NotFound(t *testing.T) {
	m := newTestManager(t)
	err := m.Delete("nonexistent")
	if err == nil {
		t.Error("expected error when deleting nonexistent image")
	}
}

func TestManager_GetAndList_WithManualEntry(t *testing.T) {
	m := newTestManager(t)

	// Manually insert an image to test Get/GetByRef/List without Docker.
	img := &loka.Image{
		ID:        "test-id-123",
		Reference: "alpine:latest",
		Status:    loka.ImageStatusReady,
	}
	m.images[img.ID] = img

	// Test Get.
	got, ok := m.Get("test-id-123")
	if !ok {
		t.Fatal("expected Get to find the image")
	}
	if got.Reference != "alpine:latest" {
		t.Errorf("expected reference alpine:latest, got %q", got.Reference)
	}

	// Test GetByRef.
	got, ok = m.GetByRef("alpine:latest")
	if !ok {
		t.Fatal("expected GetByRef to find the image")
	}
	if got.ID != "test-id-123" {
		t.Errorf("expected id test-id-123, got %q", got.ID)
	}

	// Test GetByRef with non-ready status does not match.
	img.Status = loka.ImageStatusPulling
	_, ok = m.GetByRef("alpine:latest")
	if ok {
		t.Error("expected GetByRef to not match image with non-ready status")
	}

	// Test List.
	img.Status = loka.ImageStatusReady
	imgs := m.List()
	if len(imgs) != 1 {
		t.Errorf("expected 1 image, got %d", len(imgs))
	}
}

func TestManager_Delete_Success(t *testing.T) {
	m := newTestManager(t)

	img := &loka.Image{
		ID:         "delete-me",
		Reference:  "nginx:latest",
		Status:     loka.ImageStatusReady,
		RootfsPath: "images/delete-me/rootfs.ext4",
	}
	m.images[img.ID] = img

	if err := m.Delete("delete-me"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, ok := m.Get("delete-me")
	if ok {
		t.Error("expected image to be removed after Delete")
	}

	imgs := m.List()
	if len(imgs) != 0 {
		t.Errorf("expected 0 images after delete, got %d", len(imgs))
	}
}

func TestRegister(t *testing.T) {
	m := newTestManager(t)

	img := &loka.Image{
		ID:        "reg-id-001",
		Reference: "nginx:1.25",
		Status:    loka.ImageStatusReady,
	}
	m.Register(img)

	got, ok := m.GetByRef("nginx:1.25")
	if !ok {
		t.Fatal("expected GetByRef to find the registered image")
	}
	if got.ID != "reg-id-001" {
		t.Errorf("ID = %q, want %q", got.ID, "reg-id-001")
	}
	if got.Reference != "nginx:1.25" {
		t.Errorf("Reference = %q, want %q", got.Reference, "nginx:1.25")
	}

	// Also verify via Get by ID.
	got2, ok := m.Get("reg-id-001")
	if !ok {
		t.Fatal("expected Get to find the registered image by ID")
	}
	if got2.Reference != "nginx:1.25" {
		t.Errorf("Reference = %q, want %q", got2.Reference, "nginx:1.25")
	}
}

func TestRegisterOverwrite(t *testing.T) {
	m := newTestManager(t)

	// Register first image.
	img1 := &loka.Image{
		ID:        "overwrite-id",
		Reference: "node:18",
		Status:    loka.ImageStatusReady,
		SizeMB:    100,
	}
	m.Register(img1)

	// Register second image with the same ID — should overwrite.
	img2 := &loka.Image{
		ID:        "overwrite-id",
		Reference: "node:20",
		Status:    loka.ImageStatusReady,
		SizeMB:    200,
	}
	m.Register(img2)

	got, ok := m.Get("overwrite-id")
	if !ok {
		t.Fatal("expected Get to find the image after overwrite")
	}
	if got.Reference != "node:20" {
		t.Errorf("Reference = %q, want %q (second registration should overwrite)", got.Reference, "node:20")
	}
	if got.SizeMB != 200 {
		t.Errorf("SizeMB = %d, want 200", got.SizeMB)
	}

	// The old reference should no longer match via GetByRef.
	_, ok = m.GetByRef("node:18")
	if ok {
		t.Error("expected GetByRef for old reference to return false after overwrite")
	}

	// The new reference should match.
	got2, ok := m.GetByRef("node:20")
	if !ok {
		t.Fatal("expected GetByRef for new reference to succeed")
	}
	if got2.ID != "overwrite-id" {
		t.Errorf("ID = %q, want %q", got2.ID, "overwrite-id")
	}
}

func TestManager_ResolveRootfsPath_NotFound(t *testing.T) {
	m := newTestManager(t)
	_, err := m.ResolveRootfsPath(nil, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent image")
	}
}

// ---------------------------------------------------------------------------
// Layer Ref Counting Tests
// ---------------------------------------------------------------------------

func TestLayerRefCounting_RegisterAndDelete(t *testing.T) {
	m := newTestManager(t)

	// Use digests long enough for the logger's hex[:12] slice.
	const sharedDigest = "sha256:aabbccddee001122334455667788"
	const uniqueADigest = "sha256:1111111111111111111111111111"
	const uniqueBDigest = "sha256:2222222222222222222222222222"
	// After stripping "sha256:", these become the directory names.
	sharedHex := "aabbccddee001122334455667788"
	uniqueAHex := "1111111111111111111111111111"
	uniqueBHex := "2222222222222222222222222222"

	// Create layer directories on disk.
	sharedDir := filepath.Join(m.dataDir, "layers", sharedHex)
	uniqueADir := filepath.Join(m.dataDir, "layers", uniqueAHex)
	uniqueBDir := filepath.Join(m.dataDir, "layers", uniqueBHex)
	for _, d := range []string{sharedDir, uniqueADir, uniqueBDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Register image A with layers: shared + unique-a.
	imgA := &loka.Image{
		ID:        "img-a",
		Reference: "test:a",
		Status:    loka.ImageStatusReady,
		Layers: []loka.ImageLayer{
			{Digest: sharedDigest},
			{Digest: uniqueADigest},
		},
	}
	m.mu.Lock()
	m.images[imgA.ID] = imgA
	m.layerRefs[sharedDigest]++
	m.layerRefs[uniqueADigest]++
	m.mu.Unlock()

	// Register image B with layers: shared + unique-b.
	imgB := &loka.Image{
		ID:        "img-b",
		Reference: "test:b",
		Status:    loka.ImageStatusReady,
		Layers: []loka.ImageLayer{
			{Digest: sharedDigest},
			{Digest: uniqueBDigest},
		},
	}
	m.mu.Lock()
	m.images[imgB.ID] = imgB
	m.layerRefs[sharedDigest]++
	m.layerRefs[uniqueBDigest]++
	m.mu.Unlock()

	// Delete image A.
	if err := m.Delete("img-a"); err != nil {
		t.Fatalf("Delete img-a: %v", err)
	}

	// Shared layer should still exist (ref count was 2, now 1).
	if _, err := os.Stat(sharedDir); os.IsNotExist(err) {
		t.Error("shared-digest layer dir should still exist after deleting img-a")
	}
	// unique-a should be gone (ref count was 1, now 0).
	if _, err := os.Stat(uniqueADir); !os.IsNotExist(err) {
		t.Error("unique-a layer dir should be removed after deleting img-a")
	}
	// unique-b should still exist.
	if _, err := os.Stat(uniqueBDir); os.IsNotExist(err) {
		t.Error("unique-b layer dir should still exist after deleting img-a")
	}

	// Delete image B.
	if err := m.Delete("img-b"); err != nil {
		t.Fatalf("Delete img-b: %v", err)
	}

	// Now shared layer should be gone (ref count reached 0).
	if _, err := os.Stat(sharedDir); !os.IsNotExist(err) {
		t.Error("shared-digest layer dir should be removed after deleting img-b")
	}
	// unique-b should also be gone.
	if _, err := os.Stat(uniqueBDir); !os.IsNotExist(err) {
		t.Error("unique-b layer dir should be removed after deleting img-b")
	}
}

func TestDeleteImageLocked_CleansOrphanLayers(t *testing.T) {
	m := newTestManager(t)

	// Use digests long enough for the logger's hex[:12] slice.
	const digest1 = "sha256:aaaa111111111111111111111111"
	const digest2 = "sha256:bbbb222222222222222222222222"
	hex1 := "aaaa111111111111111111111111"
	hex2 := "bbbb222222222222222222222222"

	// Create two layer directories on disk.
	layer1Dir := filepath.Join(m.dataDir, "layers", hex1)
	layer2Dir := filepath.Join(m.dataDir, "layers", hex2)
	for _, d := range []string{layer1Dir, layer2Dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Register image with both layers.
	img := &loka.Image{
		ID:        "orphan-img",
		Reference: "test:orphan",
		Status:    loka.ImageStatusReady,
		Layers: []loka.ImageLayer{
			{Digest: digest1},
			{Digest: digest2},
		},
	}
	m.mu.Lock()
	m.images[img.ID] = img
	m.layerRefs[digest1] = 1
	m.layerRefs[digest2] = 1
	// Call deleteImageLocked directly.
	m.deleteImageLocked("orphan-img")
	m.mu.Unlock()

	// Both layer dirs should be removed.
	if _, err := os.Stat(layer1Dir); !os.IsNotExist(err) {
		t.Error("layer 1 dir should be removed")
	}
	if _, err := os.Stat(layer2Dir); !os.IsNotExist(err) {
		t.Error("layer 2 dir should be removed")
	}
	// Image should be removed from map.
	if _, ok := m.Get("orphan-img"); ok {
		t.Error("image should be removed from map after deleteImageLocked")
	}
	// Layer refs should be cleaned up.
	m.mu.Lock()
	if m.layerRefs[digest1] != 0 {
		t.Errorf("layerRefs[digest1] = %d, want 0", m.layerRefs[digest1])
	}
	if m.layerRefs[digest2] != 0 {
		t.Errorf("layerRefs[digest2] = %d, want 0", m.layerRefs[digest2])
	}
	m.mu.Unlock()
}

func TestImageCleanup_FailedImagesRemoved(t *testing.T) {
	m := newTestManager(t)

	// Insert a failed image with old CreatedAt (older than 5-minute TTL).
	failedImg := &loka.Image{
		ID:        "failed-old",
		Reference: "test:failed",
		Status:    loka.ImageStatusFailed,
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	m.mu.Lock()
	m.images[failedImg.ID] = failedImg
	m.mu.Unlock()

	// Insert a ready image that should survive.
	readyImg := &loka.Image{
		ID:        "ready-keeper",
		Reference: "test:keeper",
		Status:    loka.ImageStatusReady,
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.images[readyImg.ID] = readyImg
	m.mu.Unlock()

	// Instead of waiting for the ticker, call deleteImageLocked directly
	// to simulate the cleanup behavior for the failed image.
	m.mu.Lock()
	m.deleteImageLocked("failed-old")
	m.mu.Unlock()

	// Failed image should be gone.
	if _, ok := m.Get("failed-old"); ok {
		t.Error("failed image should be removed")
	}
	// Ready image should remain.
	if _, ok := m.Get("ready-keeper"); !ok {
		t.Error("ready image should still exist")
	}
}
