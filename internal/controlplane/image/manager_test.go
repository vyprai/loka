package image

import (
	"log/slog"
	"os"
	"testing"

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
