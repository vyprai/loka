package volrepl

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/worker/volsync"
)

// ═══════════════════════════════════════════════════════════════
// Edge Cases: HTTP Handler — Error Paths
// ═══════════════════════════════════════════════════════════════

func TestHandler_GetManifest_VolumeNotFound(t *testing.T) {
	handler := NewHandler(t.TempDir(), nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/volrepl/nonexistent/manifest")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandler_GetFile_MissingPathParam(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/volrepl/vol1/file")
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandler_GetFile_FileNotFound(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/volrepl/vol1/file?path=missing.txt")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandler_PutFile_MissingPathParam(t *testing.T) {
	handler := NewHandler(t.TempDir(), nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/volrepl/vol1/file", strings.NewReader("data"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandler_DeleteFile_MissingPathParam(t *testing.T) {
	handler := NewHandler(t.TempDir(), nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/volrepl/vol1/file", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandler_DeleteFile_NonExistent(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/volrepl/vol1/file?path=ghost.txt", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	// Delete of non-existent file should still return 204 (idempotent).
	if resp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Empty Volume / Empty Files
// ═══════════════════════════════════════════════════════════════

func TestHandler_GetManifest_EmptyVolume(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "empty"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/volrepl/empty/manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestPutAndGet_EmptyFile(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Upload empty file.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/volrepl/vol1/file?path=empty.txt",
		strings.NewReader(""))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("put empty file: expected 204, got %d", resp.StatusCode)
	}

	// Download it back.
	resp, _ = http.Get(srv.URL + "/volrepl/vol1/file?path=empty.txt")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) != 0 {
		t.Errorf("expected empty body, got %d bytes", len(body))
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Client — Network Failures
// ═══════════════════════════════════════════════════════════════

func TestClient_FetchManifest_ServerDown(t *testing.T) {
	client := NewClient("http://127.0.0.1:1") // unreachable
	_, err := client.FetchManifest(context.Background(), "vol1")
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

func TestClient_DownloadFile_ServerDown(t *testing.T) {
	client := NewClient("http://127.0.0.1:1")
	err := client.DownloadFile(context.Background(), "vol1", "file.txt", filepath.Join(t.TempDir(), "f.txt"))
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

func TestClient_FetchManifest_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewClient("http://127.0.0.1:1")
	_, err := client.FetchManifest(ctx, "vol1")
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestPeerSyncTarget_UploadFile_ServerDown(t *testing.T) {
	target := NewPeerSyncTarget("http://127.0.0.1:1")
	err := target.UploadFile(context.Background(), "vol1", "f.txt", strings.NewReader("data"), 4)
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

func TestClient_FetchManifest_Server500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	_, err := client.FetchManifest(context.Background(), "vol1")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestClient_DownloadFile_Server500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	err := client.DownloadFile(context.Background(), "vol1", "f.txt", filepath.Join(t.TempDir(), "f.txt"))
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Concurrent Replication
// ═══════════════════════════════════════════════════════════════

func TestConcurrentPutToSameFile(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := strings.NewReader(strings.Repeat("x", idx+1))
			req, _ := http.NewRequest(http.MethodPost,
				srv.URL+"/volrepl/vol1/file?path=concurrent.txt", body)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	// File should exist (last writer wins, no corruption).
	data, err := os.ReadFile(filepath.Join(dataDir, "volumes", "vol1", "concurrent.txt"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if len(data) == 0 {
		t.Error("file should not be empty")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Replicator — Partial Sync
// ═══════════════════════════════════════════════════════════════

func TestPullFromPrimary_EmptyVolume(t *testing.T) {
	primaryDir := t.TempDir()
	os.MkdirAll(filepath.Join(primaryDir, "volumes", "empty"), 0o755)

	handler := NewHandler(primaryDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	replicaDir := t.TempDir()
	os.MkdirAll(filepath.Join(replicaDir, "volumes", "empty"), 0o755)

	agent := volsync.NewAgent(replicaDir, nil, nil)
	defer agent.Stop()
	r := NewReplicator(replicaDir, "r1", agent, slog.Default())
	defer r.Stop()

	client := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.pullFromPrimary(ctx, client, "empty", filepath.Join(replicaDir, "volumes", "empty")); err != nil {
		t.Fatalf("pull empty volume: %v", err)
	}
}

func TestPullFromPrimary_DeletesExtraLocalFiles(t *testing.T) {
	// Primary has file A only.
	primaryDir := t.TempDir()
	primVolDir := filepath.Join(primaryDir, "volumes", "vol1")
	os.MkdirAll(primVolDir, 0o755)
	os.WriteFile(filepath.Join(primVolDir, "keep.txt"), []byte("keep"), 0o644)

	handler := NewHandler(primaryDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Replica has files A and B (B is stale).
	replicaDir := t.TempDir()
	replVolDir := filepath.Join(replicaDir, "volumes", "vol1")
	os.MkdirAll(replVolDir, 0o755)
	os.WriteFile(filepath.Join(replVolDir, "keep.txt"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(replVolDir, "stale.txt"), []byte("stale"), 0o644)

	agent := volsync.NewAgent(replicaDir, nil, nil)
	defer agent.Stop()
	r := NewReplicator(replicaDir, "r1", agent, slog.Default())
	defer r.Stop()

	client := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r.pullFromPrimary(ctx, client, "vol1", replVolDir)

	// stale.txt should be deleted.
	if _, err := os.Stat(filepath.Join(replVolDir, "stale.txt")); !os.IsNotExist(err) {
		t.Error("stale file should have been deleted during sync")
	}

	// keep.txt should remain.
	if _, err := os.Stat(filepath.Join(replVolDir, "keep.txt")); err != nil {
		t.Error("keep.txt should still exist")
	}
}

func TestPullFromPrimary_UpdatesModifiedFiles(t *testing.T) {
	primaryDir := t.TempDir()
	primVolDir := filepath.Join(primaryDir, "volumes", "vol1")
	os.MkdirAll(primVolDir, 0o755)
	os.WriteFile(filepath.Join(primVolDir, "data.txt"), []byte("updated content"), 0o644)

	handler := NewHandler(primaryDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Replica has old version.
	replicaDir := t.TempDir()
	replVolDir := filepath.Join(replicaDir, "volumes", "vol1")
	os.MkdirAll(replVolDir, 0o755)
	os.WriteFile(filepath.Join(replVolDir, "data.txt"), []byte("old content"), 0o644)

	agent := volsync.NewAgent(replicaDir, nil, nil)
	defer agent.Stop()
	r := NewReplicator(replicaDir, "r1", agent, slog.Default())
	defer r.Stop()

	client := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r.pullFromPrimary(ctx, client, "vol1", replVolDir)

	data, _ := os.ReadFile(filepath.Join(replVolDir, "data.txt"))
	if string(data) != "updated content" {
		t.Errorf("expected updated content, got %q", data)
	}
}

func TestPullFromPrimary_PrimaryDown(t *testing.T) {
	replicaDir := t.TempDir()
	os.MkdirAll(filepath.Join(replicaDir, "volumes", "vol1"), 0o755)

	agent := volsync.NewAgent(replicaDir, nil, nil)
	defer agent.Stop()
	r := NewReplicator(replicaDir, "r1", agent, slog.Default())
	defer r.Stop()

	client := NewClient("http://127.0.0.1:1") // unreachable
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := r.pullFromPrimary(ctx, client, "vol1", filepath.Join(replicaDir, "volumes", "vol1"))
	if err == nil {
		t.Fatal("expected error when primary is unreachable")
	}
}

// ═══════════════════════════════════════════════════════════════
// Edge Cases: Replicator Lifecycle
// ═══════════════════════════════════════════════════════════════

func TestReplicator_StopVolume_Idempotent(t *testing.T) {
	agent := volsync.NewAgent(t.TempDir(), nil, nil)
	defer agent.Stop()
	r := NewReplicator(t.TempDir(), "r1", agent, slog.Default())
	defer r.Stop()

	// Stopping a volume that was never started should not panic.
	r.StopVolume("nonexistent")
	r.StopVolume("nonexistent") // double stop
}

func TestReplicator_ServePrimary_CreatesVolumeDir(t *testing.T) {
	dataDir := t.TempDir()
	agent := volsync.NewAgent(dataDir, nil, slog.Default())
	defer agent.Stop()
	r := NewReplicator(dataDir, "r1", agent, slog.Default())
	defer r.Stop()

	// Volume dir doesn't exist yet.
	err := r.ServePrimary("newvol", nil)
	if err != nil {
		t.Fatalf("ServePrimary: %v", err)
	}

	// Dir should be created.
	volDir := filepath.Join(dataDir, "volumes", "newvol")
	if _, err := os.Stat(volDir); os.IsNotExist(err) {
		t.Error("volume directory should be created")
	}

	r.StopVolume("newvol")
}

func TestReplicator_DoubleStop(t *testing.T) {
	agent := volsync.NewAgent(t.TempDir(), nil, nil)
	defer agent.Stop()
	r := NewReplicator(t.TempDir(), "r1", agent, slog.Default())

	r.Stop()
	// Second stop should not panic.
}

func TestReplicator_VolumePath(t *testing.T) {
	r := NewReplicator("/data", "r1", nil, nil)
	path := r.VolumePath("myvol")
	if path != "/data/volumes/myvol" {
		t.Errorf("expected /data/volumes/myvol, got %s", path)
	}
}
