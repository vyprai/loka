package image

import (
	"archive/tar"
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
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/pkg/lokavm"
)

const (
	imageBucket = "images"
	layerBucket = "layers"
)

// Manager handles Docker image pulling, layer extraction, and warm snapshots.
type Manager struct {
	images    map[string]*loka.Image // In-memory for now; production uses DB.
	objStore  objstore.ObjectStore
	hypervisor lokavm.Hypervisor // Hypervisor for creating warm snapshots (may be nil).
	dataDir   string      // Local cache directory.
	logger    *slog.Logger

	mu        sync.Mutex                   // Protects images map and layerRefs.
	pullLocks sync.Map                     // Per-reference dedup: ref → *pullResult.
	layerRefs map[string]int               // Layer digest → reference count from images.
}

// pullResult is used to deduplicate concurrent pulls of the same image.
type pullResult struct {
	done chan struct{}
	img  *loka.Image
	err  error
}

// NewManager creates a new image manager.
func NewManager(objStore objstore.ObjectStore, dataDir string, logger *slog.Logger) *Manager {
	os.MkdirAll(filepath.Join(dataDir, "images"), 0o755)
	m := &Manager{
		images:    make(map[string]*loka.Image),
		layerRefs: make(map[string]int),
		objStore:  objStore,
		dataDir:   dataDir,
		logger:    logger,
	}
	m.ensureLokaOverlay()

	// Start background sweep to clean up failed/stale image entries.
	go m.imageCleanupLoop()

	return m
}

// imageCleanupLoop periodically removes failed image entries and enforces
// a max image count to prevent unbounded memory growth.
func (m *Manager) imageCleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		const maxImages = 100
		const failedTTL = 5 * time.Minute

		// Remove failed images older than 5 minutes.
		for id, img := range m.images {
			if img.Status == loka.ImageStatusFailed && now.Sub(img.CreatedAt) > failedTTL {
				delete(m.images, id)
			}
		}

		// If still over limit, evict oldest ready images (least recently created).
		if len(m.images) > maxImages {
			type entry struct {
				id  string
				at  time.Time
			}
			var candidates []entry
			for id, img := range m.images {
				if img.Status == loka.ImageStatusReady {
					candidates = append(candidates, entry{id, img.CreatedAt})
				}
			}
			// Sort oldest first.
			sort.Slice(candidates, func(i, j int) bool {
				return candidates[i].at.Before(candidates[j].at)
			})
			toEvict := len(m.images) - maxImages
			for i := 0; i < toEvict && i < len(candidates); i++ {
				m.deleteImageLocked(candidates[i].id)
			}
		}

		m.mu.Unlock()
	}
}

// deleteImageLocked removes an image and decrements layer refs.
// Must be called with m.mu held.
func (m *Manager) deleteImageLocked(id string) {
	img, ok := m.images[id]
	if !ok {
		return
	}
	layersBaseDir := filepath.Join(m.dataDir, "layers")
	for _, l := range img.Layers {
		m.layerRefs[l.Digest]--
		if m.layerRefs[l.Digest] <= 0 {
			delete(m.layerRefs, l.Digest)
			hex := l.Digest
			if idx := strings.Index(hex, ":"); idx >= 0 {
				hex = hex[idx+1:]
			}
			os.RemoveAll(filepath.Join(layersBaseDir, hex))
		}
	}
	delete(m.images, id)
}

// SetHypervisor sets the hypervisor used for creating warm snapshots.
func (m *Manager) SetHypervisor(h lokavm.Hypervisor) {
	m.hypervisor = h
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

// Pull downloads an OCI image from a registry, extracts it to a plain directory,
// and injects loka-supervisor. No Docker daemon required — uses crane (pure Go).
//
// Steps:
//  1. crane.Pull() — fetch image from registry
//  2. mutate.Extract() — flatten all layers into single tar stream
//  3. Extract tar to directory (the rootfs)
//  4. Inject loka-supervisor into /usr/local/bin/
//  5. Store rootfs directory path
func (m *Manager) Pull(ctx context.Context, reference string) (*loka.Image, error) {
	// Check if already pulled.
	m.mu.Lock()
	for _, img := range m.images {
		if img.Reference == reference && img.Status == loka.ImageStatusReady {
			m.mu.Unlock()
			return img, nil
		}
	}
	m.mu.Unlock()

	// Deduplicate concurrent pulls of the same image.
	// If another goroutine is already pulling this reference, wait for it.
	pr := &pullResult{done: make(chan struct{})}
	if existing, loaded := m.pullLocks.LoadOrStore(reference, pr); loaded {
		// Another pull is in progress — wait for it.
		existingPR := existing.(*pullResult)
		<-existingPR.done
		return existingPR.img, existingPR.err
	}
	// We own this pull. Store result for waiters and clean up when done.
	var pullImg *loka.Image
	var pullErr error
	defer func() {
		pr.img = pullImg
		pr.err = pullErr
		close(pr.done)
		m.pullLocks.Delete(reference)
	}()

	id := uuid.New().String()[:12]
	img := &loka.Image{
		ID:        id,
		Reference: reference,
		Status:    loka.ImageStatusPulling,
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.images[id] = img
	m.mu.Unlock()

	m.logger.Info("pulling image", "id", id, "reference", reference)

	// Step 1: Pull image from registry (no Docker daemon needed).
	// Always pull linux/arm64 — VMs run ARM64 Linux regardless of host arch.
	remoteImg, err := crane.Pull(reference, crane.WithContext(ctx), crane.WithPlatform(&v1.Platform{
		OS: "linux", Architecture: "arm64",
	}))
	if err != nil {
		img.Status = loka.ImageStatusFailed
		pullImg, pullErr = img, fmt.Errorf("pull %s: %w", reference, err)
		return pullImg, pullErr
	}

	// Get digest.
	digest, err := remoteImg.Digest()
	if err == nil {
		img.Digest = digest.String()
	}

	// Get layers metadata.
	remoteLayers, _ := remoteImg.Layers()
	for _, l := range remoteLayers {
		d, _ := l.Digest()
		sz, _ := l.Size()
		img.Layers = append(img.Layers, loka.ImageLayer{
			Digest: d.String(),
			Size:   sz,
		})
	}

	img.Status = loka.ImageStatusConverting
	m.logger.Info("extracting layers", "id", id, "count", len(remoteLayers))

	// Step 2: Extract each layer individually (deduplicated by digest).
	// Layers are stored in $dataDir/layers/<digest>/ — shared across images.
	layersBaseDir := filepath.Join(m.dataDir, "layers")
	var layerDirs []string

	for i, l := range remoteLayers {
		d, _ := l.Digest()
		layerDir := filepath.Join(layersBaseDir, d.Hex)

		// Skip if already extracted (deduplication).
		if _, err := os.Stat(layerDir); err == nil {
			m.logger.Info("layer cached", "digest", d.Hex[:12], "index", i)
			layerDirs = append(layerDirs, layerDir)
			continue
		}

		// Extract layer tar to directory.
		os.MkdirAll(layerDir, 0o755)
		lr, err := l.Uncompressed()
		if err != nil {
			img.Status = loka.ImageStatusFailed
			pullImg, pullErr = img, fmt.Errorf("uncompress layer %d: %w", i, err)
			return pullImg, pullErr
		}
		if err := extractTarToDir(lr, layerDir); err != nil {
			lr.Close()
			img.Status = loka.ImageStatusFailed
			pullImg, pullErr = img, fmt.Errorf("extract layer %d: %w", i, err)
			return pullImg, pullErr
		}
		lr.Close()
		m.logger.Info("layer extracted", "digest", d.Hex[:12], "index", i)
		layerDirs = append(layerDirs, layerDir)
	}

	// Store layer dir paths in image metadata for VM boot.
	img.LayerPackKey = strings.Join(layerDirs, ":")

	// Step 3: Ensure loka overlay layer exists with supervisor + busybox.
	m.ensureLokaOverlay()
	lokaLayerDir := filepath.Join(m.dataDir, "layers", "loka-overlay")
	// Prepend loka layer at the bottom so image layers take precedence.
	// The overlay stacks: loka-overlay (bottom) → image layers → upper (top).
	// This ensures image binaries like /bin/sh override loka-overlay's busybox symlinks,
	// while loka-supervisor is still available at /usr/local/bin/ (not overridden by images).
	layerDirs = append([]string{lokaLayerDir}, layerDirs...)

	// Calculate total size across all layers.
	var totalSize int64
	for _, ld := range layerDirs {
		filepath.Walk(ld, func(_ string, info os.FileInfo, _ error) error {
			if info != nil { totalSize += info.Size() }
			return nil
		})
	}
	img.SizeMB = totalSize / (1024 * 1024)

	// RootfsPath = colon-separated layer dirs (bottom to top).
	// The agent passes these to the VM as virtiofs mounts for overlayfs stacking.
	img.RootfsPath = strings.Join(layerDirs, ":")

	img.Status = loka.ImageStatusReady

	// Increment layer reference counts for this image.
	m.mu.Lock()
	for _, l := range img.Layers {
		m.layerRefs[l.Digest]++
	}
	m.mu.Unlock()

	m.logger.Info("image ready",
		"id", id,
		"reference", reference,
		"size_mb", img.SizeMB,
		"layers", len(img.Layers),
		"rootfs", img.RootfsPath,
	)
	pullImg = img
	return pullImg, nil
}

// extractTarToDir extracts a tar stream to a directory, handling whiteouts.
// Hardlinks are deferred to ensure targets exist before linking.
func extractTarToDir(r io.Reader, dir string) error {
	tr := tar.NewReader(r)

	type deferredLink struct {
		oldname, newname string
		isHard           bool
	}
	var links []deferredLink

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(dir, header.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dir)) {
			continue
		}

		// OCI whiteout files.
		base := filepath.Base(header.Name)
		if strings.HasPrefix(base, ".wh.") {
			deleted := filepath.Join(filepath.Dir(target), strings.TrimPrefix(base, ".wh."))
			os.RemoveAll(deleted)
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(header.Mode)|0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				continue
			}
			io.Copy(f, tr)
			f.Close()
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target) // Remove if exists (layer override).
			os.Symlink(header.Linkname, target)
		case tar.TypeLink:
			// Defer hardlinks — target file may not be extracted yet.
			links = append(links, deferredLink{
				oldname: filepath.Join(dir, header.Linkname),
				newname: target,
				isHard:  true,
			})
		}
	}

	// Create deferred hardlinks.
	for _, link := range links {
		os.MkdirAll(filepath.Dir(link.newname), 0o755)
		os.Remove(link.newname)
		if err := os.Link(link.oldname, link.newname); err != nil {
			// Fallback: copy the file if hardlink fails (cross-device, etc).
			if src, e := os.Open(link.oldname); e == nil {
				if dst, e := os.Create(link.newname); e == nil {
					io.Copy(dst, src)
					dst.Close()
				}
				src.Close()
			}
		}
	}

	return nil
}

// ensureLokaOverlay creates or updates the loka-overlay layer with the Linux supervisor binary.
// Called at startup and during image pull.
func (m *Manager) ensureLokaOverlay() {
	lokaLayerDir := filepath.Join(m.dataDir, "layers", "loka-overlay")
	supervisorDst := filepath.Join(lokaLayerDir, "usr", "local", "bin", "loka-supervisor")

	// Check if the supervisor exists and is a valid Linux ELF binary.
	needsCreate := false
	if _, err := os.Stat(supervisorDst); os.IsNotExist(err) {
		needsCreate = true
	} else {
		// Verify it's ELF, not Mach-O.
		f, err := os.Open(supervisorDst)
		if err == nil {
			var magic [4]byte
			f.Read(magic[:])
			f.Close()
			if !(magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F') {
				needsCreate = true // Wrong binary type.
			}
		}
	}

	// Also check if the source binary has changed (updated supervisor).
	if !needsCreate {
		supervisorSrc := m.findSupervisor()
		if supervisorSrc != "" {
			srcHash := fileHash(supervisorSrc)
			dstHash := fileHash(supervisorDst)
			if srcHash != "" && dstHash != "" && srcHash != dstHash {
				needsCreate = true
				m.logger.Info("loka overlay: supervisor binary changed, updating")
			}
		}
	}

	if !needsCreate {
		return
	}

	os.MkdirAll(lokaLayerDir, 0o755)
	os.MkdirAll(filepath.Dir(supervisorDst), 0o755)

	if supervisorSrc := m.findSupervisor(); supervisorSrc != "" {
		copyFile(supervisorSrc, supervisorDst)
		os.Chmod(supervisorDst, 0o755)
		m.logger.Info("loka overlay: supervisor injected", "src", supervisorSrc)
	} else {
		m.logger.Warn("loka overlay: Linux supervisor binary not found")
	}

	// Create busybox symlinks.
	os.MkdirAll(filepath.Join(lokaLayerDir, "bin"), 0o755)
	os.MkdirAll(filepath.Join(lokaLayerDir, "sbin"), 0o755)
	os.MkdirAll(filepath.Join(lokaLayerDir, "usr/bin"), 0o755)
	for _, cmd := range []string{
		"sh", "ash", "cat", "ls", "cp", "mv", "rm", "mkdir", "rmdir",
		"echo", "printf", "test", "true", "false", "sleep", "date",
		"grep", "sed", "awk", "sort", "uniq", "wc", "head", "tail",
		"find", "xargs", "tr", "cut", "tee", "touch", "chmod", "chown",
		"ln", "readlink", "basename", "dirname", "realpath",
		"ps", "kill", "id", "whoami", "hostname", "uname", "env",
		"mount", "umount", "mknod", "df", "du",
		"tar", "gzip", "gunzip", "wget", "ping", "ip", "ifconfig", "route",
	} {
		os.Symlink("busybox", filepath.Join(lokaLayerDir, "bin", cmd))
	}
	for _, cmd := range []string{"init", "reboot", "halt", "ifconfig", "route", "ip"} {
		os.Symlink("../bin/busybox", filepath.Join(lokaLayerDir, "sbin", cmd))
	}
	for _, cmd := range []string{"env", "test"} {
		os.Symlink("../../bin/busybox", filepath.Join(lokaLayerDir, "usr/bin", cmd))
	}
	m.logger.Info("loka overlay layer created")
}

// findSupervisor locates the Linux arm64 loka-supervisor binary to inject into images.
// The supervisor runs inside Linux VMs, so we must find the Linux build, not the host (macOS) binary.
func (m *Manager) findSupervisor() string {
	arch := runtime.GOARCH // arm64 or amd64

	candidates := []string{
		// Cross-compiled Linux binary (preferred).
		filepath.Join(m.dataDir, "bin", "linux-"+arch, "loka-supervisor"),
	}

	// Check next to the lokad binary for a linux-{arch} sibling.
	if self, err := os.Executable(); err == nil {
		dir := filepath.Dir(self)
		candidates = append(candidates,
			filepath.Join(dir, "..", "linux-"+arch, "loka-supervisor"), // bin/../linux-arm64/
			filepath.Join(dir, "linux-"+arch, "loka-supervisor"),      // bin/linux-arm64/
		)
	}

	// Also check common project build output paths.
	candidates = append(candidates,
		"bin/linux-"+arch+"/loka-supervisor",
		filepath.Join(os.Getenv("HOME"), ".loka", "bin", "linux-"+arch, "loka-supervisor"),
	)

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			// Verify it's actually a Linux ELF binary, not a macOS Mach-O.
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			var magic [4]byte
			f.Read(magic[:])
			f.Close()
			if magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F' {
				return p
			}
			// Skip non-ELF binaries (macOS Mach-O, etc.)
		}
	}
	return ""
}

// fileHash returns the SHA256 hex digest of a file, or empty string on error.
func fileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// createExt4FromDir creates an ext4 image from a directory.
// Requires mkfs.ext4 (install e2fsprogs on macOS: brew install e2fsprogs).
// Falls back to local mkfs.ext4 on Linux.
func (m *Manager) createExt4FromDir(ctx context.Context, srcDir, dstPath string, contentSize int64) error {
	// Size: content + 20% overhead for ext4 metadata, minimum 64MB.
	sizeMB := (contentSize / (1024 * 1024)) * 12 / 10
	if sizeMB < 64 {
		sizeMB = 64
	}
	if sizeMB > 4096 {
		sizeMB = 4096 // Cap at 4GB.
	}

	// Try local mkfs.ext4 first (Linux).
	if _, err := exec.LookPath("mkfs.ext4"); err == nil {
		return m.createExt4Local(ctx, srcDir, dstPath, sizeMB)
	}

	return fmt.Errorf("mkfs.ext4 not found — install e2fsprogs (brew install e2fsprogs on macOS)")
}

func (m *Manager) createExt4Local(ctx context.Context, srcDir, dstPath string, sizeMB int64) error {
	// Create sparse file.
	if err := exec.CommandContext(ctx, "dd", "if=/dev/zero", "of="+dstPath, "bs=1M", fmt.Sprintf("count=0"), fmt.Sprintf("seek=%d", sizeMB)).Run(); err != nil {
		return fmt.Errorf("create sparse file: %w", err)
	}
	if err := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", dstPath).Run(); err != nil {
		return fmt.Errorf("mkfs.ext4: %w", err)
	}
	// Mount and copy.
	mountDir, _ := os.MkdirTemp("", "loka-ext4-mount-*")
	defer os.RemoveAll(mountDir)
	if err := exec.CommandContext(ctx, "mount", "-o", "loop", dstPath, mountDir).Run(); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	defer exec.CommandContext(ctx, "umount", mountDir).Run()
	if err := exec.CommandContext(ctx, "cp", "-a", srcDir+"/.", mountDir+"/").Run(); err != nil {
		return fmt.Errorf("cp: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
// Layers with no remaining references are also cleaned up from disk.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	img, ok := m.images[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("image not found")
	}

	// Decrement layer references; GC layers that reach zero.
	layersBaseDir := filepath.Join(m.dataDir, "layers")
	for _, l := range img.Layers {
		m.layerRefs[l.Digest]--
		if m.layerRefs[l.Digest] <= 0 {
			delete(m.layerRefs, l.Digest)
			// Extract hex digest from "sha256:abc123..." format.
			hex := l.Digest
			if idx := strings.Index(hex, ":"); idx >= 0 {
				hex = hex[idx+1:]
			}
			layerDir := filepath.Join(layersBaseDir, hex)
			os.RemoveAll(layerDir)
			m.logger.Info("layer GC'd (zero refs)", "digest", hex[:12])
		}
	}

	delete(m.images, id)
	m.mu.Unlock()

	// Remove layer-pack from object store.
	if img.LayerPackKey != "" {
		m.objStore.Delete(context.Background(), imageBucket, img.LayerPackKey)
	}
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

// ResolveRootfsPath returns the local rootfs directory for an image.
// With crane-based pull, this is a plain directory (not ext4).
func (m *Manager) ResolveRootfsPath(ctx context.Context, imageID string) (string, error) {
	img, ok := m.images[imageID]
	if !ok {
		return "", fmt.Errorf("image %s not found", imageID)
	}
	if img.RootfsPath == "" {
		return "", fmt.Errorf("rootfs not found for image %s", imageID)
	}
	// Layered rootfs: colon-separated layer dirs. Verify first layer exists.
	if strings.Contains(img.RootfsPath, ":") {
		first := strings.SplitN(img.RootfsPath, ":", 2)[0]
		if _, err := os.Stat(first); err == nil {
			return img.RootfsPath, nil
		}
		return "", fmt.Errorf("rootfs layer not found: %s", first)
	}
	if _, err := os.Stat(img.RootfsPath); err == nil {
		return img.RootfsPath, nil
	}
	return "", fmt.Errorf("rootfs not found for image %s", imageID)
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

	// Boot a temporary VM using the lokavm hypervisor.
	if _, cerr := m.hypervisor.CreateVM(lokavm.VMConfig{
		ID:          tmpID,
		VCPUsMin:    1,
		VCPUsMax:    1,
		MemoryMinMB: 128,
		MemoryMaxMB: 512,
		BootArgs:    "console=hvc0 init=/usr/local/bin/loka-supervisor",
		Vsock:       true,
	}); cerr != nil {
		return "", "", fmt.Errorf("create temp VM: %w", cerr)
	}
	defer m.hypervisor.DeleteVM(tmpID)

	if err := m.hypervisor.StartVM(tmpID); err != nil {
		return "", "", fmt.Errorf("start temp VM: %w", err)
	}

	m.logger.Info("temp VM started, creating snapshot", "image", img.ID)

	// Create snapshot (pauses the VM).
	snap, err := m.hypervisor.CreateSnapshot(tmpID)
	if err != nil {
		return "", "", fmt.Errorf("create diff snapshot: %w", err)
	}

	// Upload snapshot files if they exist.
	if snap.MemPath != "" {
		memKey = fmt.Sprintf("images/%s/snapshot_mem.gz", img.ID)
		if err := m.gzipUpload(ctx, snap.MemPath, imageBucket, memKey); err != nil {
			return "", "", fmt.Errorf("upload snapshot mem: %w", err)
		}
	}
	if snap.StatePath != "" {
		stateKey = fmt.Sprintf("images/%s/snapshot_vmstate.gz", img.ID)
		if err := m.gzipUpload(ctx, snap.StatePath, imageBucket, stateKey); err != nil {
			return "", "", fmt.Errorf("upload snapshot vmstate: %w", err)
		}
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
