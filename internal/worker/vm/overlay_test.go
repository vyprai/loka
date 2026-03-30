package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func setupTestOverlay(t *testing.T) (*OverlayManager, string) {
	t.Helper()
	dir := t.TempDir()
	mgr := NewOverlayManager(dir)
	sid := "test-session"
	if err := mgr.Init(sid); err != nil {
		t.Fatal(err)
	}
	return mgr, sid
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFullDiff_Added(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	// Snapshot A: empty.
	writeFile(t, filepath.Join(ws, "existing.txt"), "hello")
	layerA, _ := mgr.CreateLayer(sid)

	// Add files.
	writeFile(t, filepath.Join(ws, "new.txt"), "new content")
	writeFile(t, filepath.Join(ws, "sub/deep.txt"), "deep")
	layerB, _ := mgr.CreateLayer(sid)

	summary, err := mgr.FullDiff(sid, layerA, layerB)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Added < 2 {
		t.Errorf("added = %d, want >= 2 (files + possible dirs)", summary.Added)
	}

	// Check that new files are in the entries.
	found := map[string]bool{}
	for _, e := range summary.Entries {
		if e.Type == DiffAdded {
			found[e.Path] = true
		}
	}
	if !found["new.txt"] {
		t.Error("new.txt should be in added entries")
	}
}

func TestFullDiff_Modified(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	writeFile(t, filepath.Join(ws, "data.txt"), "version 1")
	layerA, _ := mgr.CreateLayer(sid)

	writeFile(t, filepath.Join(ws, "data.txt"), "version 2 - different content")
	layerB, _ := mgr.CreateLayer(sid)

	summary, err := mgr.FullDiff(sid, layerA, layerB)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Modified != 1 {
		t.Errorf("modified = %d, want 1", summary.Modified)
	}

	// Check hash differs.
	for _, e := range summary.Entries {
		if e.Path == "data.txt" && e.Type == DiffModified {
			if e.Hash == e.OldHash {
				t.Error("hashes should differ for modified file")
			}
			if e.OldSize == e.Size {
				t.Error("sizes should differ")
			}
			return
		}
	}
	t.Error("data.txt not found in modified entries")
}

func TestFullDiff_Deleted(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	writeFile(t, filepath.Join(ws, "keep.txt"), "keep")
	writeFile(t, filepath.Join(ws, "remove.txt"), "remove me")
	layerA, _ := mgr.CreateLayer(sid)

	os.Remove(filepath.Join(ws, "remove.txt"))
	layerB, _ := mgr.CreateLayer(sid)

	summary, err := mgr.FullDiff(sid, layerA, layerB)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", summary.Deleted)
	}

	for _, e := range summary.Entries {
		if e.Path == "remove.txt" {
			if e.Type != DiffDeleted {
				t.Errorf("remove.txt type = %s, want deleted", e.Type)
			}
			if e.OldSize == 0 {
				t.Error("deleted file should have old_size")
			}
			return
		}
	}
	t.Error("remove.txt not in entries")
}

func TestFullDiff_ModeChange(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	writeFile(t, filepath.Join(ws, "script.sh"), "#!/bin/sh\necho hi")
	layerA, _ := mgr.CreateLayer(sid)

	os.Chmod(filepath.Join(ws, "script.sh"), 0o755)
	layerB, _ := mgr.CreateLayer(sid)

	summary, err := mgr.FullDiff(sid, layerA, layerB)
	if err != nil {
		t.Fatal(err)
	}

	if summary.ModeChanged != 1 {
		t.Errorf("mode_changed = %d, want 1", summary.ModeChanged)
	}
}

func TestFullDiff_Mixed(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	writeFile(t, filepath.Join(ws, "keep.txt"), "unchanged")
	writeFile(t, filepath.Join(ws, "modify.txt"), "v1")
	writeFile(t, filepath.Join(ws, "delete.txt"), "gone soon")
	layerA, _ := mgr.CreateLayer(sid)

	// Modify, delete, and add.
	writeFile(t, filepath.Join(ws, "modify.txt"), "v2 changed")
	os.Remove(filepath.Join(ws, "delete.txt"))
	writeFile(t, filepath.Join(ws, "add.txt"), "brand new")
	layerB, _ := mgr.CreateLayer(sid)

	summary, err := mgr.FullDiff(sid, layerA, layerB)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Added != 1 {
		t.Errorf("added = %d, want 1", summary.Added)
	}
	if summary.Modified != 1 {
		t.Errorf("modified = %d, want 1", summary.Modified)
	}
	if summary.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", summary.Deleted)
	}

	// Unchanged file should NOT appear.
	for _, e := range summary.Entries {
		if e.Path == "keep.txt" {
			t.Error("unchanged file should not appear in diff")
		}
	}
}

func TestFullDiff_IdenticalLayers(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	writeFile(t, filepath.Join(ws, "same.txt"), "identical")
	layerA, _ := mgr.CreateLayer(sid)
	layerB, _ := mgr.CreateLayer(sid)

	summary, err := mgr.FullDiff(sid, layerA, layerB)
	if err != nil {
		t.Fatal(err)
	}

	if len(summary.Entries) != 0 {
		t.Errorf("identical layers should have 0 entries, got %d", len(summary.Entries))
	}
}

// ---------------------------------------------------------------------------
// Overlay CoW Clone Tests
// ---------------------------------------------------------------------------

func TestCreateLayer_PreservesContent(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	writeFile(t, filepath.Join(ws, "hello.txt"), "world")
	writeFile(t, filepath.Join(ws, "sub/nested.txt"), "deep")

	layerName, err := mgr.CreateLayer(sid)
	if err != nil {
		t.Fatal(err)
	}

	// Verify layer content matches workspace.
	layerDir := filepath.Join(mgr.SessionDir(sid), "layers", layerName)

	got, err := os.ReadFile(filepath.Join(layerDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt from layer: %v", err)
	}
	if string(got) != "world" {
		t.Errorf("hello.txt = %q, want %q", got, "world")
	}

	got, err = os.ReadFile(filepath.Join(layerDir, "sub/nested.txt"))
	if err != nil {
		t.Fatalf("read sub/nested.txt from layer: %v", err)
	}
	if string(got) != "deep" {
		t.Errorf("sub/nested.txt = %q, want %q", got, "deep")
	}
}

func TestRestoreLayer_RestoresContent(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	// Write original content and create layer.
	writeFile(t, filepath.Join(ws, "data.txt"), "original")
	writeFile(t, filepath.Join(ws, "keep.txt"), "preserved")

	layerName, err := mgr.CreateLayer(sid)
	if err != nil {
		t.Fatal(err)
	}

	// Modify workspace after checkpoint.
	writeFile(t, filepath.Join(ws, "data.txt"), "modified")
	writeFile(t, filepath.Join(ws, "new-file.txt"), "should disappear")
	os.Remove(filepath.Join(ws, "keep.txt"))

	// Verify modifications took effect.
	got, _ := os.ReadFile(filepath.Join(ws, "data.txt"))
	if string(got) != "modified" {
		t.Fatalf("data.txt should be modified before restore, got %q", got)
	}

	// Restore the layer.
	if err := mgr.RestoreLayer(sid, layerName); err != nil {
		t.Fatal(err)
	}

	// Verify workspace matches original state.
	got, err = os.ReadFile(filepath.Join(ws, "data.txt"))
	if err != nil {
		t.Fatalf("read data.txt after restore: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("data.txt = %q after restore, want %q", got, "original")
	}

	got, err = os.ReadFile(filepath.Join(ws, "keep.txt"))
	if err != nil {
		t.Fatalf("read keep.txt after restore: %v", err)
	}
	if string(got) != "preserved" {
		t.Errorf("keep.txt = %q after restore, want %q", got, "preserved")
	}

	// new-file.txt should not exist after restore.
	if _, err := os.Stat(filepath.Join(ws, "new-file.txt")); !os.IsNotExist(err) {
		t.Error("new-file.txt should not exist after restore")
	}
}

func TestCloneDir_ContentMatch(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	os.MkdirAll(srcDir, 0o755)
	os.MkdirAll(dstDir, 0o755)

	// Create source files.
	writeFile(t, filepath.Join(srcDir, "root.txt"), "root content")
	writeFile(t, filepath.Join(srcDir, "sub/child.txt"), "child content")
	writeFile(t, filepath.Join(srcDir, "sub/deep/leaf.txt"), "leaf content")

	// Clone.
	if err := cloneDir(srcDir, dstDir); err != nil {
		t.Fatal(err)
	}

	// Verify all files match.
	checks := map[string]string{
		"root.txt":          "root content",
		"sub/child.txt":     "child content",
		"sub/deep/leaf.txt": "leaf content",
	}
	for rel, want := range checks {
		got, err := os.ReadFile(filepath.Join(dstDir, rel))
		if err != nil {
			t.Fatalf("read %s from clone: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

func TestCreateLayer_MultipleCheckpoints(t *testing.T) {
	mgr, sid := setupTestOverlay(t)
	ws := mgr.WorkspacePath(sid)

	// Checkpoint 1: single file.
	writeFile(t, filepath.Join(ws, "v1.txt"), "version 1")
	layer1, err := mgr.CreateLayer(sid)
	if err != nil {
		t.Fatal(err)
	}

	// Checkpoint 2: add another file.
	writeFile(t, filepath.Join(ws, "v2.txt"), "version 2")
	layer2, err := mgr.CreateLayer(sid)
	if err != nil {
		t.Fatal(err)
	}

	// Checkpoint 3: modify first file.
	writeFile(t, filepath.Join(ws, "v1.txt"), "version 1 updated")
	layer3, err := mgr.CreateLayer(sid)
	if err != nil {
		t.Fatal(err)
	}

	// Verify layer1 has only v1.txt with original content.
	l1Dir := filepath.Join(mgr.SessionDir(sid), "layers", layer1)
	got, err := os.ReadFile(filepath.Join(l1Dir, "v1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version 1" {
		t.Errorf("layer1 v1.txt = %q, want %q", got, "version 1")
	}
	if _, err := os.Stat(filepath.Join(l1Dir, "v2.txt")); !os.IsNotExist(err) {
		t.Error("layer1 should not contain v2.txt")
	}

	// Verify layer2 has both files.
	l2Dir := filepath.Join(mgr.SessionDir(sid), "layers", layer2)
	got, err = os.ReadFile(filepath.Join(l2Dir, "v1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version 1" {
		t.Errorf("layer2 v1.txt = %q, want %q", got, "version 1")
	}
	got, err = os.ReadFile(filepath.Join(l2Dir, "v2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version 2" {
		t.Errorf("layer2 v2.txt = %q, want %q", got, "version 2")
	}

	// Verify layer3 has updated v1.txt.
	l3Dir := filepath.Join(mgr.SessionDir(sid), "layers", layer3)
	got, err = os.ReadFile(filepath.Join(l3Dir, "v1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version 1 updated" {
		t.Errorf("layer3 v1.txt = %q, want %q", got, "version 1 updated")
	}

	// All three layers should be listed.
	layers, err := mgr.ListLayers(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 {
		t.Errorf("got %d layers, want 3", len(layers))
	}
}

func TestDiffDirs_CrossSession(t *testing.T) {
	dir := t.TempDir()
	dirA := filepath.Join(dir, "a")
	dirB := filepath.Join(dir, "b")
	os.MkdirAll(dirA, 0o755)
	os.MkdirAll(dirB, 0o755)

	writeFile(t, filepath.Join(dirA, "shared.txt"), "same content")
	writeFile(t, filepath.Join(dirB, "shared.txt"), "same content")
	writeFile(t, filepath.Join(dirA, "only-a.txt"), "only in A")
	writeFile(t, filepath.Join(dirB, "only-b.txt"), "only in B")

	summary, err := DiffDirs(dirA, dirB)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Added != 1 { // only-b.txt
		t.Errorf("added = %d, want 1", summary.Added)
	}
	if summary.Deleted != 1 { // only-a.txt
		t.Errorf("deleted = %d, want 1", summary.Deleted)
	}
	if summary.Modified != 0 { // shared.txt is identical
		t.Errorf("modified = %d, want 0", summary.Modified)
	}
}
