// Package volsync provides a volume synchronization agent that watches local
// volume directories for changes and syncs them to/from an object store.
//
// Design: local-first. Files live on the worker's disk for fast virtiofs access.
// Changes are detected via fsnotify and uploaded to objstore immediately.
// Other workers pull changes when notified by the control plane.
package volsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vyprai/loka/internal/objstore"
)

const (
	volumeBucket = "volumes"
	manifestKey  = ".lokavol/manifest.json"
)

// SyncTarget abstracts where synced data goes (objstore or peer worker).
type SyncTarget interface {
	UploadFile(ctx context.Context, volName, relPath string, r io.Reader, size int64) error
	DeleteFile(ctx context.Context, volName, relPath string) error
	SaveManifest(ctx context.Context, volName string, manifest *Manifest) error
	FetchManifest(ctx context.Context, volName string) (*Manifest, error)
	DownloadFile(ctx context.Context, volName, relPath, localPath string) error
}

// Agent watches local volume directories and syncs changes to objstore.
type Agent struct {
	dataDir  string // Root data directory (volumes at {dataDir}/volumes/).
	objStore objstore.ObjectStore
	logger   *slog.Logger

	mu      sync.Mutex
	watches map[string]*volumeWatch // volume name → watch state
	// targets holds additional sync targets per volume (e.g., peer workers).
	// The objStore target is always used implicitly when objStore != nil.
	targets map[string][]SyncTarget // volume name → extra targets
	ctx     context.Context
	cancel  context.CancelFunc
}

// Manifest tracks file state for efficient sync.
type Manifest struct {
	Version int                    `json:"version"`
	Files   map[string]FileEntry   `json:"files"`
}

// FileEntry describes a single file in the manifest.
type FileEntry struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MTime  string `json:"mtime"`
}

type volumeWatch struct {
	name       string
	localDir   string
	watcher    *fsnotify.Watcher
	manifest   *Manifest
	manifestMu sync.RWMutex // Protects manifest reads and writes.
	cancel     context.CancelFunc
}

// NewAgent creates a volume sync agent.
func NewAgent(dataDir string, objStore objstore.ObjectStore, logger *slog.Logger) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		dataDir:  dataDir,
		objStore: objStore,
		logger:   logger,
		watches:  make(map[string]*volumeWatch),
		targets:  make(map[string][]SyncTarget),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// AddTarget registers an additional sync target for a volume (e.g., peer worker).
func (a *Agent) AddTarget(volName string, target SyncTarget) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.targets[volName] = append(a.targets[volName], target)
}

// RemoveTargets removes all extra sync targets for a volume.
func (a *Agent) RemoveTargets(volName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.targets, volName)
}

// BuildLocalManifest scans a local directory and builds a manifest.
func BuildLocalManifest(volDir string) *Manifest {
	return buildLocalManifest(volDir)
}

// HashFile computes the SHA256 hash of a file.
func HashFile(path string) (string, error) {
	return hashFile(path)
}

// Stop shuts down all watchers.
func (a *Agent) Stop() {
	a.cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, w := range a.watches {
		w.watcher.Close()
		w.cancel()
		delete(a.watches, name)
	}
}

// WatchVolume starts watching a volume directory for changes.
// Changes are synced to objstore immediately via fsnotify.
func (a *Agent) WatchVolume(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.watches[name]; ok {
		return nil // Already watching.
	}

	volDir := filepath.Join(a.dataDir, "volumes", name)
	if _, err := os.Stat(volDir); os.IsNotExist(err) {
		return fmt.Errorf("volume directory does not exist: %s", volDir)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	// Watch the volume directory and all subdirectories.
	if err := watchRecursive(watcher, volDir); err != nil {
		watcher.Close()
		return fmt.Errorf("watch directory: %w", err)
	}

	ctx, cancel := context.WithCancel(a.ctx)
	w := &volumeWatch{
		name:     name,
		localDir: volDir,
		watcher:  watcher,
		manifest: &Manifest{Version: 1, Files: make(map[string]FileEntry)},
		cancel:   cancel,
	}
	a.watches[name] = w

	// Load existing manifest from objstore if available.
	a.loadManifest(ctx, w)

	// Start the watch loop.
	go a.watchLoop(ctx, w)

	a.logger.Info("volume watch started", "volume", name, "path", volDir)
	return nil
}

// UnwatchVolume stops watching a volume.
func (a *Agent) UnwatchVolume(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if w, ok := a.watches[name]; ok {
		w.cancel()
		w.watcher.Close()
		delete(a.watches, name)
		a.logger.Info("volume watch stopped", "volume", name)
	}
}

// SyncToRemote uploads all local files to objstore for a volume.
func (a *Agent) SyncToRemote(name string) error {
	volDir := filepath.Join(a.dataDir, "volumes", name)
	if _, err := os.Stat(volDir); os.IsNotExist(err) {
		return nil // Nothing to sync.
	}

	count := 0
	err := filepath.Walk(volDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(volDir, path)
		if strings.HasPrefix(rel, ".lokavol") {
			return nil // Skip manifest metadata.
		}
		if err := a.uploadFile(name, volDir, rel); err != nil {
			a.logger.Warn("sync upload failed", "volume", name, "file", rel, "error", err)
		} else {
			count++
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Save manifest.
	a.mu.Lock()
	w := a.watches[name]
	a.mu.Unlock()
	if w != nil {
		a.saveManifest(w)
	}

	if count > 0 {
		a.logger.Info("volume synced to remote", "volume", name, "files", count)
	}
	return nil
}

// SyncFromRemote downloads files from objstore that are newer or missing locally.
func (a *Agent) SyncFromRemote(name string) error {
	if a.objStore == nil {
		return nil
	}

	volDir := filepath.Join(a.dataDir, "volumes", name)
	os.MkdirAll(volDir, 0o755)

	// Get remote manifest.
	remoteManifest, err := a.fetchRemoteManifest(name)
	if err != nil || remoteManifest == nil {
		return nil // No remote data.
	}

	// Build local manifest for comparison.
	localManifest := buildLocalManifest(volDir)

	// Download files that differ.
	count := 0
	for relPath, remoteEntry := range remoteManifest.Files {
		localEntry, exists := localManifest.Files[relPath]
		if exists && localEntry.SHA256 == remoteEntry.SHA256 {
			continue // Same content.
		}
		if err := a.downloadFile(name, volDir, relPath); err != nil {
			a.logger.Warn("sync download failed", "volume", name, "file", relPath, "error", err)
		} else {
			count++
		}
	}

	if count > 0 {
		a.logger.Info("volume synced from remote", "volume", name, "files", count)
	}
	return nil
}

// watchLoop handles fsnotify events and syncs changed files.
func (a *Agent) watchLoop(ctx context.Context, w *volumeWatch) {
	// Debounce: collect events for a short window before syncing.
	var pending []string
	timer := time.NewTimer(time.Hour) // Start with an inactive timer.
	timer.Stop()

	// Periodic full reconciliation catches events fsnotify may have missed
	// under heavy load. Runs every 5 minutes.
	reconcileTicker := time.NewTicker(5 * time.Minute)
	defer reconcileTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-reconcileTicker.C:
			a.reconcileVolume(w)

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			rel, err := filepath.Rel(w.localDir, event.Name)
			if err != nil || strings.HasPrefix(rel, ".lokavol") {
				continue
			}

			// Add newly created directories to the watcher.
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					w.watcher.Add(event.Name)
				}
			}

			pending = append(pending, rel)
			timer.Reset(200 * time.Millisecond) // Debounce 200ms.

		case <-timer.C:
			if len(pending) == 0 {
				continue
			}
			// Sync all pending files.
			seen := make(map[string]bool)
			for _, rel := range pending {
				if seen[rel] {
					continue
				}
				seen[rel] = true

				fullPath := filepath.Join(w.localDir, rel)
				if _, err := os.Stat(fullPath); os.IsNotExist(err) {
					// File deleted — remove from objstore.
					a.deleteRemoteFile(w.name, rel)
					w.manifestMu.Lock()
					delete(w.manifest.Files, rel)
					w.manifestMu.Unlock()
				} else {
					// File created/modified — upload.
					if err := a.uploadFile(w.name, w.localDir, rel); err != nil {
						a.logger.Warn("sync upload failed", "volume", w.name, "file", rel, "error", err)
					}
				}
			}
			pending = pending[:0]
			a.saveManifest(w)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			a.logger.Warn("fsnotify error", "volume", w.name, "error", err)
		}
	}
}

// reconcileVolume does a full directory walk to catch changes that fsnotify may
// have missed (e.g., under heavy I/O load). Uploads new/changed files and
// removes deleted entries from the manifest.
func (a *Agent) reconcileVolume(w *volumeWatch) {
	if a.objStore == nil {
		return
	}

	// Walk the local directory and compute hashes.
	currentFiles := make(map[string]bool)
	filepath.Walk(w.localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(w.localDir, path)
		if relErr != nil || strings.HasPrefix(rel, ".lokavol") {
			return nil
		}
		currentFiles[rel] = true

		// Check if file has changed since last manifest entry.
		w.manifestMu.RLock()
		existing, ok := w.manifest.Files[rel]
		w.manifestMu.RUnlock()
		if ok && existing.Size == info.Size() && existing.MTime == info.ModTime().UTC().Format(time.RFC3339) {
			return nil // Unchanged.
		}

		// Upload changed file (uploadFile acquires manifestMu internally).
		if err := a.uploadFile(w.name, w.localDir, rel); err != nil {
			a.logger.Warn("reconcile upload failed", "volume", w.name, "file", rel, "error", err)
		}
		return nil
	})

	// Remove manifest entries for deleted files.
	changed := false
	w.manifestMu.Lock()
	for rel := range w.manifest.Files {
		if !currentFiles[rel] {
			a.deleteRemoteFile(w.name, rel)
			delete(w.manifest.Files, rel)
			changed = true
		}
	}
	w.manifestMu.Unlock()
	if changed {
		a.saveManifest(w)
	}
}

// uploadFile uploads a single file to objstore and extra targets, updates the manifest.
func (a *Agent) uploadFile(volName, volDir, relPath string) error {
	fullPath := filepath.Join(volDir, relPath)
	info, err := os.Stat(fullPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}

	// Upload to objstore if available.
	if a.objStore != nil {
		f, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		objKey := volName + "/" + relPath
		err = a.objStore.Put(a.ctx, volumeBucket, objKey, f, info.Size())
		f.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", relPath, err)
		}
	}

	// Push to extra sync targets (peer workers) — async, best-effort.
	a.mu.Lock()
	targets := a.targets[volName]
	a.mu.Unlock()
	for _, t := range targets {
		f, err := os.Open(fullPath)
		if err != nil {
			continue
		}
		if err := t.UploadFile(a.ctx, volName, relPath, f, info.Size()); err != nil {
			a.logger.Warn("target upload failed", "volume", volName, "file", relPath, "error", err)
		}
		f.Close()
	}

	// Update manifest entry.
	hash, _ := hashFile(fullPath)
	a.mu.Lock()
	w := a.watches[volName]
	a.mu.Unlock()
	if w != nil {
		w.manifestMu.Lock()
		w.manifest.Files[relPath] = FileEntry{
			Size:   info.Size(),
			SHA256: hash,
			MTime:  info.ModTime().UTC().Format(time.RFC3339),
		}
		w.manifestMu.Unlock()
	}

	return nil
}

// downloadFile downloads a single file from objstore to local volume dir.
func (a *Agent) downloadFile(volName, volDir, relPath string) error {
	objKey := volName + "/" + relPath
	reader, err := a.objStore.Get(a.ctx, volumeBucket, objKey)
	if err != nil {
		return fmt.Errorf("download %s: %w", relPath, err)
	}
	defer reader.Close()

	fullPath := filepath.Join(volDir, relPath)
	os.MkdirAll(filepath.Dir(fullPath), 0o755)

	// Atomic write: write to temp file, then rename into place.
	// This prevents partial/corrupted files on interrupted downloads.
	tmpPath := fullPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	if _, err = io.Copy(f, reader); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, fullPath)
}

// deleteRemoteFile removes a file from objstore and extra targets.
func (a *Agent) deleteRemoteFile(volName, relPath string) {
	if a.objStore != nil {
		objKey := volName + "/" + relPath
		a.objStore.Delete(a.ctx, volumeBucket, objKey)
	}
	a.mu.Lock()
	targets := a.targets[volName]
	a.mu.Unlock()
	for _, t := range targets {
		t.DeleteFile(a.ctx, volName, relPath)
	}
}

// saveManifest writes the volume manifest to objstore and extra targets.
func (a *Agent) saveManifest(w *volumeWatch) {
	w.manifestMu.RLock()
	data, err := json.MarshalIndent(w.manifest, "", "  ")
	w.manifestMu.RUnlock()
	if err != nil {
		return
	}
	if a.objStore != nil {
		reader := strings.NewReader(string(data))
		objKey := w.name + "/" + manifestKey
		a.objStore.Put(a.ctx, volumeBucket, objKey, reader, int64(len(data)))
	}
	a.mu.Lock()
	targets := a.targets[w.name]
	a.mu.Unlock()
	for _, t := range targets {
		t.SaveManifest(a.ctx, w.name, w.manifest)
	}
}

// loadManifest loads the manifest from objstore into the watch state.
func (a *Agent) loadManifest(ctx context.Context, w *volumeWatch) {
	if a.objStore == nil {
		return
	}
	objKey := w.name + "/" + manifestKey
	reader, err := a.objStore.Get(ctx, volumeBucket, objKey)
	if err != nil {
		return // No manifest yet.
	}
	defer reader.Close()
	w.manifestMu.Lock()
	json.NewDecoder(reader).Decode(w.manifest)
	w.manifestMu.Unlock()
}

// fetchRemoteManifest downloads and parses the remote manifest.
func (a *Agent) fetchRemoteManifest(volName string) (*Manifest, error) {
	objKey := volName + "/" + manifestKey
	reader, err := a.objStore.Get(a.ctx, volumeBucket, objKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var m Manifest
	if err := json.NewDecoder(reader).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// buildLocalManifest scans a local directory and builds a manifest.
func buildLocalManifest(volDir string) *Manifest {
	m := &Manifest{Version: 1, Files: make(map[string]FileEntry)}
	filepath.Walk(volDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(volDir, path)
		if strings.HasPrefix(rel, ".lokavol") {
			return nil
		}
		hash, _ := hashFile(path)
		m.Files[rel] = FileEntry{
			Size:   info.Size(),
			SHA256: hash,
			MTime:  info.ModTime().UTC().Format(time.RFC3339),
		}
		return nil
	})
	return m
}

// watchRecursive adds a directory and all its subdirectories to the watcher.
func watchRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return watcher.Add(path)
		}
		return nil
	})
}

// hashFile computes the SHA256 hash of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
