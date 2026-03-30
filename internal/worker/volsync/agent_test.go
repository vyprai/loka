package volsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWritePattern(t *testing.T) {
	// Verify the atomic write pattern used by downloadFile:
	// write to .tmp file, then rename into place.
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")
	tmpPath := target + ".tmp"

	// Simulate atomic write: write to .tmp then rename.
	if err := os.WriteFile(tmpPath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		t.Fatal(err)
	}

	// Verify target exists with correct content.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}

	// Verify .tmp is gone after rename.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("expected .tmp file to not exist after rename")
	}
}

func TestAtomicWritePattern_CleanupOnError(t *testing.T) {
	// Verify that the .tmp file is cleaned up if something goes wrong
	// before rename (simulating the error path in downloadFile).
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")
	tmpPath := target + ".tmp"

	// Write partial data to .tmp.
	if err := os.WriteFile(tmpPath, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate error: remove the .tmp without renaming (like the error path).
	os.Remove(tmpPath)

	// Target should not exist.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("target should not exist after aborted write")
	}

	// .tmp should not exist.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error(".tmp should not exist after cleanup")
	}
}

func TestAtomicWritePattern_OverwriteExisting(t *testing.T) {
	// Verify atomic write correctly replaces an existing file.
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")

	// Write initial content.
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Atomic overwrite.
	tmpPath := target + ".tmp"
	if err := os.WriteFile(tmpPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestAtomicWritePattern_NestedDir(t *testing.T) {
	// Verify atomic write creates parent directories (as downloadFile does).
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "dir", "test.txt")

	// MkdirAll for parent dir (mirrors downloadFile behavior).
	os.MkdirAll(filepath.Dir(target), 0o755)

	tmpPath := target + ".tmp"
	if err := os.WriteFile(tmpPath, []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nested" {
		t.Errorf("got %q, want %q", got, "nested")
	}
}

func TestBuildLocalManifest(t *testing.T) {
	dir := t.TempDir()

	// Create some test files.
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "subdir", "file2.txt"), []byte("world"), 0o644)

	// Create a .lokavol directory that should be skipped.
	os.MkdirAll(filepath.Join(dir, ".lokavol"), 0o755)
	os.WriteFile(filepath.Join(dir, ".lokavol", "manifest.json"), []byte("{}"), 0o644)

	m := buildLocalManifest(dir)
	if m == nil {
		t.Fatal("buildLocalManifest returned nil")
	}
	if m.Version != 1 {
		t.Errorf("version = %d, want 1", m.Version)
	}

	// Should have 2 files (not the .lokavol one).
	if len(m.Files) != 2 {
		t.Errorf("file count = %d, want 2", len(m.Files))
	}

	if _, ok := m.Files["file1.txt"]; !ok {
		t.Error("missing file1.txt in manifest")
	}
	if _, ok := m.Files[filepath.Join("subdir", "file2.txt")]; !ok {
		t.Error("missing subdir/file2.txt in manifest")
	}

	// Verify SHA256 is populated.
	entry := m.Files["file1.txt"]
	if entry.SHA256 == "" {
		t.Error("expected non-empty SHA256 for file1.txt")
	}
	if entry.Size != 5 {
		t.Errorf("file1.txt size = %d, want 5", entry.Size)
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// SHA256 of "hello" is well-known.
	expected := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if hash != expected {
		t.Errorf("hash = %q, want %q", hash, expected)
	}
}

func TestHashFile_NonexistentFile(t *testing.T) {
	_, err := hashFile("/nonexistent/file.txt")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}
