package volsync

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestWatchVolume_NonexistentDir(t *testing.T) {
	agent := NewAgent(t.TempDir(), nil, slog.Default())
	defer agent.Stop()

	err := agent.WatchVolume("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent volume directory")
	}
}

func TestWatchVolume_AlreadyWatching(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()

	agent.WatchVolume("vol1")
	err := agent.WatchVolume("vol1") // Second watch — should return nil (idempotent).
	if err != nil {
		t.Fatalf("second WatchVolume should be idempotent, got %v", err)
	}
}

func TestUnwatchVolume_NotWatched(t *testing.T) {
	agent := NewAgent(t.TempDir(), nil, slog.Default())
	defer agent.Stop()

	// Should not panic.
	agent.UnwatchVolume("nonexistent")
}

func TestSyncToRemote_NilObjStore(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "vol1")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file.txt"), []byte("data"), 0o644)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()

	// With nil objStore, should not panic.
	err := agent.SyncToRemote("vol1")
	if err != nil {
		t.Fatalf("SyncToRemote with nil objStore should not error: %v", err)
	}
}

func TestSyncToRemote_NonexistentVolume(t *testing.T) {
	agent := NewAgent(t.TempDir(), nil, slog.Default())
	defer agent.Stop()

	err := agent.SyncToRemote("ghost")
	if err != nil {
		t.Fatalf("SyncToRemote on missing volume should be no-op, got %v", err)
	}
}

func TestSyncFromRemote_NilObjStore(t *testing.T) {
	agent := NewAgent(t.TempDir(), nil, slog.Default())
	defer agent.Stop()

	err := agent.SyncFromRemote("vol1")
	if err != nil {
		t.Fatalf("SyncFromRemote with nil objStore should be no-op: %v", err)
	}
}

func TestBuildLocalManifest_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := BuildLocalManifest(dir)
	if len(m.Files) != 0 {
		t.Errorf("empty dir should produce empty manifest, got %d files", len(m.Files))
	}
}

func TestBuildLocalManifest_SkipsLokavolDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".lokavol"), 0o755)
	os.WriteFile(filepath.Join(dir, ".lokavol", "manifest.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte("x"), 0o644)

	m := BuildLocalManifest(dir)
	if _, ok := m.Files["data.txt"]; !ok {
		t.Error("expected data.txt in manifest")
	}
	for path := range m.Files {
		if path == ".lokavol/manifest.json" {
			t.Error(".lokavol files should be skipped")
		}
	}
}

func TestBuildLocalManifest_NestedDirs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "c", "deep.txt"), []byte("nested"), 0o644)
	os.WriteFile(filepath.Join(dir, "top.txt"), []byte("top"), 0o644)

	m := BuildLocalManifest(dir)
	if len(m.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(m.Files))
	}
}

func TestHashFile_NonexistentFile_Error(t *testing.T) {
	_, err := HashFile("/nonexistent/file.txt")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestHashFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	os.WriteFile(path, nil, 0o644)

	hash, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// SHA256 of empty file is the well-known hash.
	if hash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("wrong hash for empty file: %s", hash)
	}
}

func TestHashFile_ConsistentHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	os.WriteFile(path, []byte("hello"), 0o644)

	h1, _ := HashFile(path)
	h2, _ := HashFile(path)
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %s != %s", h1, h2)
	}
}

func TestUploadFile_NilObjStore_WithTargets(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "vol1")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file.txt"), []byte("data"), 0o644)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()
	agent.WatchVolume("vol1")

	// Upload with nil objStore — should still update manifest.
	err := agent.uploadFile("vol1", volDir, "file.txt")
	if err != nil {
		t.Fatalf("uploadFile with nil objStore: %v", err)
	}

	// Check manifest was updated.
	agent.mu.Lock()
	w := agent.watches["vol1"]
	agent.mu.Unlock()

	w.manifestMu.RLock()
	_, exists := w.manifest.Files["file.txt"]
	w.manifestMu.RUnlock()

	if !exists {
		t.Error("manifest should contain file.txt after upload")
	}
}

func TestUploadFile_Directory_Skipped(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "vol1")
	os.MkdirAll(filepath.Join(volDir, "subdir"), 0o755)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()
	agent.WatchVolume("vol1")

	err := agent.uploadFile("vol1", volDir, "subdir")
	if err != nil {
		t.Fatalf("uploading a directory should be silently skipped: %v", err)
	}
}

func TestUploadFile_DeletedFile(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "vol1")
	os.MkdirAll(volDir, 0o755)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()
	agent.WatchVolume("vol1")

	// Upload file that doesn't exist.
	err := agent.uploadFile("vol1", volDir, "deleted.txt")
	if err == nil {
		t.Fatal("expected error for deleted file")
	}
}
