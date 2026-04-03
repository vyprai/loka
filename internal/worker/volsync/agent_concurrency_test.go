package volsync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestManifestConcurrentReadWrite(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "concurrent")
	os.MkdirAll(volDir, 0o755)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()

	// Create volume and some initial files.
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(volDir, fmt.Sprintf("file-%d.txt", i)), []byte("data"), 0o644)
	}

	if err := agent.WatchVolume("concurrent"); err != nil {
		t.Fatalf("WatchVolume: %v", err)
	}

	// Hammer the manifest from multiple goroutines.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			fname := fmt.Sprintf("concurrent-%d.txt", idx)
			fpath := filepath.Join(volDir, fname)

			// Write file → triggers manifest update via uploadFile.
			os.WriteFile(fpath, []byte(fmt.Sprintf("data-%d", idx)), 0o644)
			agent.uploadFile("concurrent", volDir, fname)
		}(i)
	}

	// Concurrent reads via BuildLocalManifest.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			BuildLocalManifest(volDir)
		}()
	}

	// Concurrent SyncToRemote (reads manifest).
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent.SyncToRemote("concurrent")
		}()
	}

	wg.Wait()

	// Verify no panic occurred and manifest is consistent.
	agent.mu.Lock()
	w := agent.watches["concurrent"]
	agent.mu.Unlock()

	if w == nil {
		t.Fatal("watch should exist")
	}

	w.manifestMu.RLock()
	fileCount := len(w.manifest.Files)
	w.manifestMu.RUnlock()

	// Should have at least some files tracked.
	if fileCount == 0 {
		t.Error("expected at least some files in manifest")
	}
}

func TestManifestConcurrentDeleteAndUpload(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "delup")
	os.MkdirAll(volDir, 0o755)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()

	// Pre-populate files.
	for i := 0; i < 20; i++ {
		os.WriteFile(filepath.Join(volDir, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o644)
	}
	agent.WatchVolume("delup")

	var wg sync.WaitGroup

	// Half goroutines upload new files.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			fname := fmt.Sprintf("new-%d.txt", idx)
			os.WriteFile(filepath.Join(volDir, fname), []byte("new"), 0o644)
			agent.uploadFile("delup", volDir, fname)
		}(i)
	}

	// Other half delete files from manifest directly.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			agent.mu.Lock()
			w := agent.watches["delup"]
			agent.mu.Unlock()
			if w != nil {
				w.manifestMu.Lock()
				delete(w.manifest.Files, fmt.Sprintf("f%d.txt", idx))
				w.manifestMu.Unlock()
			}
		}(i)
	}

	wg.Wait()
	// No panic = success.
}

func TestReconcileVolumeConcurrentWithWatchLoop(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "recon")
	os.MkdirAll(volDir, 0o755)

	for i := 0; i < 50; i++ {
		os.WriteFile(filepath.Join(volDir, fmt.Sprintf("data-%d.bin", i)), []byte("payload"), 0o644)
	}

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()
	agent.WatchVolume("recon")

	agent.mu.Lock()
	w := agent.watches["recon"]
	agent.mu.Unlock()

	var wg sync.WaitGroup

	// Simulate reconcile running concurrently.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent.reconcileVolume(w)
		}()
	}

	// Simultaneously write new files.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			fname := fmt.Sprintf("new-recon-%d.txt", idx)
			os.WriteFile(filepath.Join(volDir, fname), []byte("new"), 0o644)
			agent.uploadFile("recon", volDir, fname)
		}(i)
	}

	wg.Wait()
}

func TestSaveManifestConcurrent(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "savem")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file.txt"), []byte("x"), 0o644)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()
	agent.WatchVolume("savem")

	agent.mu.Lock()
	w := agent.watches["savem"]
	agent.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent.saveManifest(w)
		}()
	}
	wg.Wait()
}

func TestWatchUnwatchConcurrent(t *testing.T) {
	dataDir := t.TempDir()

	for i := 0; i < 5; i++ {
		volDir := filepath.Join(dataDir, "volumes", fmt.Sprintf("vol%d", i))
		os.MkdirAll(volDir, 0o755)
		os.WriteFile(filepath.Join(volDir, "data.txt"), []byte("x"), 0o644)
	}

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			agent.WatchVolume(fmt.Sprintf("vol%d", idx))
		}(i)
		go func(idx int) {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			agent.UnwatchVolume(fmt.Sprintf("vol%d", idx))
		}(i)
	}
	wg.Wait()
}

func TestSyncTargetsConcurrent(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "targets")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file.txt"), []byte("x"), 0o644)

	agent := NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()

	// Add/remove targets concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			agent.AddTarget("targets", &noopSyncTarget{})
		}()
		go func() {
			defer wg.Done()
			agent.RemoveTargets("targets")
		}()
	}
	wg.Wait()
}

// noopSyncTarget is a test SyncTarget that does nothing.
type noopSyncTarget struct{}

func (n *noopSyncTarget) UploadFile(_ context.Context, _, _ string, _ io.Reader, _ int64) error {
	return nil
}
func (n *noopSyncTarget) DeleteFile(_ context.Context, _, _ string) error           { return nil }
func (n *noopSyncTarget) SaveManifest(_ context.Context, _ string, _ *Manifest) error { return nil }
func (n *noopSyncTarget) FetchManifest(_ context.Context, _ string) (*Manifest, error) {
	return nil, nil
}
func (n *noopSyncTarget) DownloadFile(_ context.Context, _, _, _ string) error { return nil }
