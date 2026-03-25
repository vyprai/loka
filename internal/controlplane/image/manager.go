package image

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
)

const (
	imageBucket = "images"
	layerBucket = "layers"
)

// Manager handles Docker image pulling, layer extraction, and warm snapshots.
type Manager struct {
	images    map[string]*loka.Image // In-memory for now; production uses DB.
	objStore  objstore.ObjectStore
	vmManager *vm.Manager // VM manager for creating warm snapshots (may be nil).
	dataDir   string      // Local cache directory.
	logger    *slog.Logger
}

// NewManager creates a new image manager.
func NewManager(objStore objstore.ObjectStore, dataDir string, logger *slog.Logger) *Manager {
	os.MkdirAll(filepath.Join(dataDir, "images"), 0o755)
	return &Manager{
		images:   make(map[string]*loka.Image),
		objStore: objStore,
		dataDir:  dataDir,
		logger:   logger,
	}
}

// SetVMManager sets the VM manager used for creating warm snapshots.
// Must be called before Pull if warm snapshots are desired.
func (m *Manager) SetVMManager(vmMgr *vm.Manager) {
	m.vmManager = vmMgr
}

// dockerSaveManifest represents one entry in the manifest.json produced
// by `docker save`. Each entry maps a config blob, repo tags, and an
// ordered list of layer tar paths (bottom-to-top).
type dockerSaveManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// layerInfo is an internal type used during layer extraction.
// TarPath is ephemeral and not persisted to the Image struct.
type layerInfo struct {
	loka.ImageLayer
	TarPath string // Absolute path to the extracted layer tar (temporary).
}

// Pull downloads a Docker image, extracts its layers, builds a layer-pack
// ext4, and uploads everything to the object store.
//
// Steps:
//  1. docker pull <reference>
//  2. docker save <reference> > image.tar (preserves layers)
//  3. Extract and parse manifest.json to discover layers
//  4. For each layer: compute digest, create ext4, upload (deduplicated)
//  5. Build layer-pack ext4 (all layers as /0/, /1/, /2/...)
//  6. Inject loka-supervisor into the top layer
//  7. Upload layer-pack to object store
//  8. Optionally: boot in Firecracker and create warm snapshot
func (m *Manager) Pull(ctx context.Context, reference string) (*loka.Image, error) {
	// Check if already pulled.
	for _, img := range m.images {
		if img.Reference == reference && img.Status == loka.ImageStatusReady {
			return img, nil
		}
	}

	id := uuid.New().String()[:12]
	img := &loka.Image{
		ID:        id,
		Reference: reference,
		Status:    loka.ImageStatusPulling,
		CreatedAt: time.Now(),
	}
	m.images[id] = img

	m.logger.Info("pulling image", "id", id, "reference", reference)

	// Step 1: Pull the Docker image.
	if err := runCmd(ctx, "docker", "pull", reference); err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("docker pull: %w", err)
	}

	// Get digest.
	digest, _ := cmdOutput(ctx, "docker", "inspect", "--format={{index .RepoDigests 0}}", reference)
	img.Digest = strings.TrimSpace(digest)

	// Step 2: docker save to get layered tar.
	img.Status = loka.ImageStatusConverting

	tmpDir, err := os.MkdirTemp("", "loka-layers-*")
	if err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	savePath := filepath.Join(tmpDir, "image.tar")
	if err := runCmd(ctx, "docker", "save", "-o", savePath, reference); err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("docker save: %w", err)
	}

	// Step 3: Extract and parse manifest to discover layers.
	layers, err := m.extractLayers(ctx, savePath, tmpDir)
	if err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("extract layers: %w", err)
	}

	m.logger.Info("extracted layers", "id", id, "count", len(layers))

	// Step 4: Upload each layer (deduplicated by digest).
	for i := range layers {
		layer := &layers[i]
		exists, existsErr := m.objStore.Exists(ctx, layerBucket, layer.ObjKey)
		if existsErr != nil {
			m.logger.Warn("checking layer existence failed, will re-upload",
				"digest", layer.Digest, "error", existsErr)
		}
		if exists {
			m.logger.Info("layer already exists, skipping upload", "digest", layer.Digest)
			continue
		}

		f, openErr := os.Open(layer.TarPath)
		if openErr != nil {
			img.Status = loka.ImageStatusFailed
			return img, fmt.Errorf("open layer tar %s: %w", layer.Digest, openErr)
		}
		info, _ := os.Stat(layer.TarPath)
		if uploadErr := m.objStore.Put(ctx, layerBucket, layer.ObjKey, f, info.Size()); uploadErr != nil {
			f.Close()
			img.Status = loka.ImageStatusFailed
			return img, fmt.Errorf("upload layer %s: %w", layer.Digest, uploadErr)
		}
		f.Close()
		m.logger.Info("uploaded layer", "digest", layer.Digest, "size", layer.Size)
	}

	// Step 5: Build layer-pack ext4 (all layers combined).
	layerPackPath := filepath.Join(tmpDir, "layer-pack.ext4")
	if err := m.buildLayerPack(ctx, layers, layerPackPath, reference); err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("build layer-pack: %w", err)
	}

	// Step 6: Upload layer-pack to object store.
	img.LayerPackKey = fmt.Sprintf("images/%s/layer-pack.ext4", id)
	packInfo, _ := os.Stat(layerPackPath)
	if packInfo != nil {
		img.SizeMB = packInfo.Size() / (1024 * 1024)
	}

	packFile, err := os.Open(layerPackPath)
	if err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("open layer-pack: %w", err)
	}
	if err := m.objStore.Put(ctx, imageBucket, img.LayerPackKey, packFile, packInfo.Size()); err != nil {
		packFile.Close()
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("upload layer-pack: %w", err)
	}
	packFile.Close()

	// Populate image metadata.
	imgLayers := make([]loka.ImageLayer, len(layers))
	for i, l := range layers {
		imgLayers[i] = l.ImageLayer
	}
	img.Layers = imgLayers
	img.RootfsPath = img.LayerPackKey // Backward compatibility for existing callers.

	// Step 7: Create warm snapshot for fast future boots.
	if m.vmManager != nil {
		img.Status = loka.ImageStatusWarming
		memKey, stateKey, snapErr := m.createWarmSnapshot(ctx, img, layerPackPath)
		if snapErr != nil {
			m.logger.Warn("warm snapshot failed, cold boot will be used",
				"id", id, "error", snapErr)
		} else {
			img.SnapshotMem = memKey
			img.SnapshotVMState = stateKey
			m.logger.Info("warm snapshot created",
				"id", id, "mem_key", memKey, "state_key", stateKey)
		}
	}

	img.Status = loka.ImageStatusReady
	m.logger.Info("image ready",
		"id", id,
		"reference", reference,
		"size_mb", img.SizeMB,
		"layers", len(img.Layers),
		"warm_snapshot", img.SnapshotMem != "",
	)
	return img, nil
}

// extractLayers parses a `docker save` tar archive and returns layer metadata.
// The docker save format contains a manifest.json with an array of entries,
// each listing layer tar paths like "abc123def456/layer.tar".
func (m *Manager) extractLayers(ctx context.Context, savePath, tmpDir string) ([]layerInfo, error) {
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return nil, fmt.Errorf("create extract dir: %w", err)
	}

	// Extract the docker save tar.
	if err := runCmd(ctx, "tar", "xf", savePath, "-C", extractDir); err != nil {
		return nil, fmt.Errorf("extract image tar: %w", err)
	}

	// Parse manifest.json.
	manifestData, err := os.ReadFile(filepath.Join(extractDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}

	var manifests []dockerSaveManifest
	if err := json.Unmarshal(manifestData, &manifests); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("manifest.json contains no entries")
	}

	manifest := manifests[0]
	layers := make([]layerInfo, 0, len(manifest.Layers))

	for _, layerPath := range manifest.Layers {
		fullPath := filepath.Join(extractDir, layerPath)

		// Compute SHA256 digest of the layer tar.
		digest, err := computeSHA256(fullPath)
		if err != nil {
			return nil, fmt.Errorf("compute digest for %s: %w", layerPath, err)
		}

		info, err := os.Stat(fullPath)
		if err != nil {
			return nil, fmt.Errorf("stat layer %s: %w", layerPath, err)
		}

		layers = append(layers, layerInfo{
			ImageLayer: loka.ImageLayer{
				Digest: "sha256:" + digest,
				Size:   info.Size(),
				ObjKey: fmt.Sprintf("sha256:%s/layer.tar", digest),
			},
			TarPath: fullPath,
		})
	}

	return layers, nil
}

// buildLayerPack creates a single ext4 image containing all layers as
// numbered directories (/0/, /1/, /2/, ...) ordered bottom-to-top.
// The loka-supervisor binary is injected into the top layer.
func (m *Manager) buildLayerPack(ctx context.Context, layers []layerInfo, outputPath, reference string) error {
	// Calculate total size needed: sum of layer tars + overhead.
	var totalSize int64
	for _, l := range layers {
		totalSize += l.Size
	}
	sizeMB := totalSize/(1024*1024) + 256 // layers + filesystem overhead
	if sizeMB < 512 {
		sizeMB = 512
	}

	// Create sparse ext4 image.
	if err := runCmd(ctx, "truncate", "-s", fmt.Sprintf("%dM", sizeMB), outputPath); err != nil {
		return fmt.Errorf("create sparse image: %w", err)
	}
	if err := runCmd(ctx, "mkfs.ext4", "-F", outputPath); err != nil {
		return fmt.Errorf("mkfs: %w", err)
	}

	// Mount the ext4 image and extract each layer into numbered directories.
	mountDir := filepath.Join(filepath.Dir(outputPath), "mount-pack")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return fmt.Errorf("create mount dir: %w", err)
	}
	if err := runCmd(ctx, "sudo", "mount", "-o", "loop", outputPath, mountDir); err != nil {
		return fmt.Errorf("mount layer-pack: %w", err)
	}
	defer runCmd(ctx, "sudo", "umount", mountDir)

	for i, layer := range layers {
		layerDir := filepath.Join(mountDir, fmt.Sprintf("%d", i))
		if err := runCmd(ctx, "sudo", "mkdir", "-p", layerDir); err != nil {
			return fmt.Errorf("create layer dir %d: %w", i, err)
		}
		if err := runCmd(ctx, "sudo", "tar", "xf", layer.TarPath, "-C", layerDir); err != nil {
			return fmt.Errorf("extract layer %d (%s): %w", i, layer.Digest, err)
		}
	}

	// Inject loka-supervisor into the top layer.
	if len(layers) > 0 {
		topLayer := filepath.Join(mountDir, fmt.Sprintf("%d", len(layers)-1))
		supervisorSrc := findSupervisorBinary()
		if supervisorSrc != "" {
			dst := filepath.Join(topLayer, "usr/local/bin/loka-supervisor")
			runCmd(ctx, "sudo", "mkdir", "-p", filepath.Dir(dst))
			runCmd(ctx, "sudo", "cp", supervisorSrc, dst)
			runCmd(ctx, "sudo", "chmod", "+x", dst)
		}
	}

	return nil
}

// Get returns an image by ID.
func (m *Manager) Get(id string) (*loka.Image, bool) {
	img, ok := m.images[id]
	return img, ok
}

// Register adds an image directly (used for testing and pre-cached images).
func (m *Manager) Register(img *loka.Image) {
	m.images[img.ID] = img
}

// GetByRef returns an image by Docker reference.
func (m *Manager) GetByRef(reference string) (*loka.Image, bool) {
	for _, img := range m.images {
		if img.Reference == reference && img.Status == loka.ImageStatusReady {
			return img, true
		}
	}
	return nil, false
}

// List returns all images.
func (m *Manager) List() []*loka.Image {
	imgs := make([]*loka.Image, 0, len(m.images))
	for _, img := range m.images {
		imgs = append(imgs, img)
	}
	return imgs
}

// Delete removes an image, its layer-pack, and warm snapshot files.
// Individual layers are NOT deleted because they may be shared with other images.
func (m *Manager) Delete(id string) error {
	img, ok := m.images[id]
	if !ok {
		return fmt.Errorf("image not found")
	}
	// Remove layer-pack from object store.
	if img.LayerPackKey != "" {
		m.objStore.Delete(context.Background(), imageBucket, img.LayerPackKey)
	}
	// Also clean up via RootfsPath for backward compat (may be same as LayerPackKey).
	if img.RootfsPath != "" && img.RootfsPath != img.LayerPackKey {
		m.objStore.Delete(context.Background(), imageBucket, img.RootfsPath)
	}
	// Remove warm snapshot files from object store.
	if img.SnapshotMem != "" {
		m.objStore.Delete(context.Background(), imageBucket, img.SnapshotMem)
	}
	if img.SnapshotVMState != "" {
		m.objStore.Delete(context.Background(), imageBucket, img.SnapshotVMState)
	}
	// Remove local cache (includes layer-pack and snapshot cache).
	os.RemoveAll(filepath.Join(m.dataDir, "images", id))
	os.RemoveAll(filepath.Join(m.dataDir, "cache", "images", id))
	delete(m.images, id)
	return nil
}

// ResolveLayerPackPath returns the local cache path for an image's layer-pack
// ext4, downloading from object store on cache miss.
func (m *Manager) ResolveLayerPackPath(ctx context.Context, imageID string) (string, error) {
	img, ok := m.images[imageID]
	if !ok {
		return "", fmt.Errorf("image %s not found", imageID)
	}

	localPath := filepath.Join(m.dataDir, "cache", "images", imageID, "layer-pack.ext4")
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil // Cache hit.
	}

	// Determine the object store key to download.
	objKey := img.LayerPackKey
	if objKey == "" {
		objKey = img.RootfsPath // Backward compat: old images use RootfsPath.
	}
	if objKey == "" {
		return "", fmt.Errorf("image %s has no layer-pack or rootfs key", imageID)
	}

	// Download from object store.
	reader, err := m.objStore.Get(ctx, imageBucket, objKey)
	if err != nil {
		return "", fmt.Errorf("download layer-pack: %w", err)
	}
	defer reader.Close()

	os.MkdirAll(filepath.Dir(localPath), 0o755)
	f, err := os.Create(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := f.ReadFrom(reader); err != nil {
		return "", err
	}

	return localPath, nil
}

// ResolveRootfsPath is an alias for ResolveLayerPackPath, maintaining backward
// compatibility for callers that reference the rootfs path.
func (m *Manager) ResolveRootfsPath(ctx context.Context, imageID string) (string, error) {
	return m.ResolveLayerPackPath(ctx, imageID)
}

// ResolveSnapshotPaths returns local cache paths for an image's warm snapshot
// files, downloading and decompressing from object store on cache miss.
// Returns ("", "", nil) if the image has no warm snapshot.
func (m *Manager) ResolveSnapshotPaths(ctx context.Context, imageID string) (memPath, statePath string, err error) {
	img, ok := m.images[imageID]
	if !ok {
		return "", "", fmt.Errorf("image %s not found", imageID)
	}
	if img.SnapshotMem == "" || img.SnapshotVMState == "" {
		return "", "", nil // No warm snapshot available.
	}

	cacheDir := filepath.Join(m.dataDir, "cache", "images", imageID)
	os.MkdirAll(cacheDir, 0o755)

	memPath = filepath.Join(cacheDir, "snapshot_mem")
	statePath = filepath.Join(cacheDir, "snapshot_vmstate")

	// Download and decompress memory file if not cached.
	if _, err := os.Stat(memPath); os.IsNotExist(err) {
		if dlErr := m.downloadGunzip(ctx, imageBucket, img.SnapshotMem, memPath); dlErr != nil {
			return "", "", fmt.Errorf("download snapshot mem: %w", dlErr)
		}
	}

	// Download and decompress vmstate file if not cached.
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		if dlErr := m.downloadGunzip(ctx, imageBucket, img.SnapshotVMState, statePath); dlErr != nil {
			return "", "", fmt.Errorf("download snapshot vmstate: %w", dlErr)
		}
	}

	return memPath, statePath, nil
}

// createWarmSnapshot boots a temporary VM from the image layer-pack, waits for
// the supervisor to become ready, creates a diff snapshot, compresses and
// uploads the snapshot files to objstore, then kills the temporary VM.
func (m *Manager) createWarmSnapshot(ctx context.Context, img *loka.Image, layerPackPath string) (memKey, stateKey string, err error) {
	tmpID := fmt.Sprintf("warmsnap-%s", img.ID)

	m.logger.Info("creating warm snapshot", "image", img.ID, "tmp_vm", tmpID)

	// Boot the temporary VM using the layer-pack as rootfs.
	microVM, err := m.vmManager.Launch(ctx, tmpID, vm.VMConfig{
		VCPU:       1,
		MemoryMB:   512,
		RootfsPath: layerPackPath,
	})
	if err != nil {
		return "", "", fmt.Errorf("launch temp VM: %w", err)
	}
	// Always clean up the temporary VM.
	defer m.vmManager.Stop(tmpID)

	// Wait for supervisor to become ready via vsock ping.
	vsock := vm.NewVsockClient(microVM.VsockPath)
	backoff := 100 * time.Millisecond
	supervisorReady := false
	for i := 0; i < 50; i++ {
		if err := vsock.Ping(); err == nil {
			supervisorReady = true
			break
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}
	if !supervisorReady {
		return "", "", fmt.Errorf("supervisor did not respond to ping within timeout")
	}

	m.logger.Info("supervisor ready, creating diff snapshot", "image", img.ID)

	// Create diff snapshot (pauses the VM).
	memPath, statePath, err := m.vmManager.CreateDiffSnapshot(tmpID)
	if err != nil {
		return "", "", fmt.Errorf("create diff snapshot: %w", err)
	}

	// Compress and upload memory file.
	memKey = fmt.Sprintf("images/%s/snapshot_mem.gz", img.ID)
	if err := m.gzipUpload(ctx, memPath, imageBucket, memKey); err != nil {
		return "", "", fmt.Errorf("upload snapshot mem: %w", err)
	}

	// Compress and upload vmstate file.
	stateKey = fmt.Sprintf("images/%s/snapshot_vmstate.gz", img.ID)
	if err := m.gzipUpload(ctx, statePath, imageBucket, stateKey); err != nil {
		return "", "", fmt.Errorf("upload snapshot vmstate: %w", err)
	}

	return memKey, stateKey, nil
}

// gzipUpload compresses a local file with gzip and uploads it to objstore.
func (m *Manager) gzipUpload(ctx context.Context, localPath, bucket, key string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	// Compress to a temp file first so we know the size.
	tmpFile, err := os.CreateTemp("", "loka-snap-*.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	gw := gzip.NewWriter(tmpFile)
	if _, err := io.Copy(gw, src); err != nil {
		tmpFile.Close()
		return err
	}
	if err := gw.Close(); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	// Upload the compressed file.
	info, err := os.Stat(tmpFile.Name())
	if err != nil {
		return err
	}
	f, err := os.Open(tmpFile.Name())
	if err != nil {
		return err
	}
	defer f.Close()

	return m.objStore.Put(ctx, bucket, key, f, info.Size())
}

// downloadGunzip downloads a gzipped file from objstore and decompresses it
// to the given local path.
func (m *Manager) downloadGunzip(ctx context.Context, bucket, key, localPath string) error {
	reader, err := m.objStore.Get(ctx, bucket, key)
	if err != nil {
		return err
	}
	defer reader.Close()

	gr, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()

	os.MkdirAll(filepath.Dir(localPath), 0o755)
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, gr); err != nil {
		return err
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	return string(out), err
}

// computeSHA256 returns the hex-encoded SHA256 digest of a file.
func computeSHA256(path string) (string, error) {
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

func findSupervisorBinary() string {
	candidates := []string{
		"bin/linux-amd64/loka-supervisor",
		"/usr/local/bin/loka-supervisor",
		"bin/loka-supervisor",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
