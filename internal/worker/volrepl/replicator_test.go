package volrepl

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/worker/volsync"
)

var testLogger = slog.Default()

func TestHandler_GetManifest(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "testvol")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file1.txt"), []byte("hello"), 0o644)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/volrepl/testvol/manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var m volsync.Manifest
	json.NewDecoder(resp.Body).Decode(&m)
	if _, ok := m.Files["file1.txt"]; !ok {
		t.Error("expected file1.txt in manifest")
	}
}

func TestHandler_GetFile(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "testvol")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file1.txt"), []byte("hello world"), 0o644)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/volrepl/testvol/file?path=file1.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello world" {
		t.Errorf("expected 'hello world', got %q", body)
	}
}

func TestHandler_PutFile(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "testvol"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := strings.NewReader("file content")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/volrepl/testvol/file?path=sub/newfile.txt", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Verify file was created.
	data, err := os.ReadFile(filepath.Join(dataDir, "volumes", "testvol", "sub", "newfile.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "file content" {
		t.Errorf("expected 'file content', got %q", data)
	}
}

func TestHandler_DeleteFile(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "testvol")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "todelete.txt"), []byte("bye"), 0o644)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/volrepl/testvol/file?path=todelete.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	if _, err := os.Stat(filepath.Join(volDir, "todelete.txt")); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}
}

func TestHandler_PathTraversal(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "testvol"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/volrepl/testvol/file?path=../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for path traversal, got %d", resp.StatusCode)
	}
}

func TestClient_FetchManifest(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "testvol")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "data.bin"), []byte("data"), 0o644)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(srv.URL)
	m, err := client.FetchManifest(context.Background(), "testvol")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Files["data.bin"]; !ok {
		t.Error("expected data.bin in manifest")
	}
}

func TestClient_DownloadFile(t *testing.T) {
	dataDir := t.TempDir()
	volDir := filepath.Join(dataDir, "volumes", "testvol")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "file.txt"), []byte("content"), 0o644)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(srv.URL)
	localPath := filepath.Join(t.TempDir(), "downloaded.txt")
	if err := client.DownloadFile(context.Background(), "testvol", "file.txt", localPath); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(localPath)
	if string(data) != "content" {
		t.Errorf("expected 'content', got %q", data)
	}
}

func TestPeerSyncTarget_UploadDownload(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "volumes", "vol1"), 0o755)

	handler := NewHandler(dataDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	target := NewPeerSyncTarget(srv.URL)

	// Upload a file.
	err := target.UploadFile(context.Background(), "vol1", "uploaded.txt",
		strings.NewReader("uploaded data"), 13)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	// Verify it's on the server.
	data, _ := os.ReadFile(filepath.Join(dataDir, "volumes", "vol1", "uploaded.txt"))
	if string(data) != "uploaded data" {
		t.Errorf("expected 'uploaded data', got %q", data)
	}

	// Download it back.
	localPath := filepath.Join(t.TempDir(), "back.txt")
	if err := target.DownloadFile(context.Background(), "vol1", "uploaded.txt", localPath); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	data, _ = os.ReadFile(localPath)
	if string(data) != "uploaded data" {
		t.Errorf("expected 'uploaded data', got %q", data)
	}

	// Delete it.
	if err := target.DeleteFile(context.Background(), "vol1", "uploaded.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "volumes", "vol1", "uploaded.txt")); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}
}

func TestReplicator_PullFromPrimary(t *testing.T) {
	// Set up a "primary" worker with data.
	primaryDir := t.TempDir()
	volDir := filepath.Join(primaryDir, "volumes", "sharedvol")
	os.MkdirAll(volDir, 0o755)
	os.WriteFile(filepath.Join(volDir, "doc.txt"), []byte("primary data"), 0o644)
	os.WriteFile(filepath.Join(volDir, "sub", "deep.txt"), []byte("nested"), 0o644)
	os.MkdirAll(filepath.Join(volDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(volDir, "sub", "deep.txt"), []byte("nested"), 0o644)

	handler := NewHandler(primaryDir, nil)
	mux := http.NewServeMux()
	handler.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Set up replica.
	replicaDir := t.TempDir()
	replicaVolDir := filepath.Join(replicaDir, "volumes", "sharedvol")
	os.MkdirAll(replicaVolDir, 0o755)

	agent := volsync.NewAgent(replicaDir, nil, nil)
	defer agent.Stop()

	r := NewReplicator(replicaDir, "replica-1", agent, testLogger)
	defer r.Stop()

	// Pull from primary.
	client := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.pullFromPrimary(ctx, client, "sharedvol", replicaVolDir); err != nil {
		t.Fatalf("pullFromPrimary: %v", err)
	}

	// Verify files replicated.
	data, err := os.ReadFile(filepath.Join(replicaVolDir, "doc.txt"))
	if err != nil {
		t.Fatalf("read replicated file: %v", err)
	}
	if string(data) != "primary data" {
		t.Errorf("expected 'primary data', got %q", data)
	}

	data, err = os.ReadFile(filepath.Join(replicaVolDir, "sub", "deep.txt"))
	if err != nil {
		t.Fatalf("read nested file: %v", err)
	}
	if string(data) != "nested" {
		t.Errorf("expected 'nested', got %q", data)
	}
}
