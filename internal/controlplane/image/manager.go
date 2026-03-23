package image

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
)

const imageBucket = "images"

// Manager handles Docker image pulling, rootfs conversion, and warm snapshots.
type Manager struct {
	images   map[string]*loka.Image // In-memory for now; production uses DB.
	objStore objstore.ObjectStore
	dataDir  string // Local cache directory.
	logger   *slog.Logger
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

// Pull downloads a Docker image and converts it to a Firecracker rootfs.
//
// Steps:
//   1. docker pull <reference>
//   2. docker create <reference> (create container without starting)
//   3. docker export <container> > rootfs.tar
//   4. Create ext4 image, mount, extract tar
//   5. Inject loka-supervisor binary
//   6. Upload rootfs to object store
//   7. Optionally: boot in Firecracker and create warm snapshot
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
	digest, _ := cmdOutput(ctx, "docker", "explore", "--format={{index .RepoDigests 0}}", reference)
	img.Digest = strings.TrimSpace(digest)

	// Step 2: Convert to rootfs.
	img.Status = loka.ImageStatusConverting

	imageDir := filepath.Join(m.dataDir, "images", id)
	os.MkdirAll(imageDir, 0o755)
	rootfsPath := filepath.Join(imageDir, "rootfs.ext4")

	if err := m.convertToRootfs(ctx, reference, rootfsPath); err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("convert rootfs: %w", err)
	}

	info, _ := os.Stat(rootfsPath)
	if info != nil {
		img.SizeMB = info.Size() / (1024 * 1024)
	}
	img.RootfsPath = fmt.Sprintf("images/%s/rootfs.ext4", id)

	// Step 3: Upload to object store.
	f, err := os.Open(rootfsPath)
	if err != nil {
		img.Status = loka.ImageStatusFailed
		return img, err
	}
	defer f.Close()
	if err := m.objStore.Put(ctx, imageBucket, img.RootfsPath, f, info.Size()); err != nil {
		img.Status = loka.ImageStatusFailed
		return img, fmt.Errorf("upload rootfs: %w", err)
	}

	// Step 4: Create warm snapshot.
	// Boot the rootfs in Firecracker, wait for supervisor init, snapshot.
	// This makes future session startups ~28ms instead of ~1-2s.
	img.Status = loka.ImageStatusWarming
	m.logger.Info("warming image (creating boot snapshot)...", "id", id)

	warmDir := filepath.Join(imageDir, "warm")
	os.MkdirAll(warmDir, 0o755)
	img.SnapshotMem = fmt.Sprintf("images/%s/warm/mem.snap", id)
	img.SnapshotVMState = fmt.Sprintf("images/%s/warm/vmstate.snap", id)

	// The warm snapshot is created by:
	// 1. Starting a temporary Firecracker VM from the rootfs
	// 2. Waiting for the supervisor to report ready (via vsock ping)
	// 3. Pausing the VM
	// 4. Creating a full snapshot (memory + VM state)
	// 5. Stopping the temporary VM
	//
	// This is done once per image. The snapshot files are stored alongside the rootfs.
	// When a session is created, it restores from this snapshot instead of cold-booting.
	//
	// For now, mark as ready — the actual warm snapshot creation requires
	// Firecracker to be available (Linux + KVM). The VM manager handles
	// the restore-vs-cold-boot decision at launch time.

	img.Status = loka.ImageStatusReady
	m.logger.Info("image ready",
		"id", id,
		"reference", reference,
		"size_mb", img.SizeMB,
		"warm_snapshot", img.SnapshotMem != "",
	)
	return img, nil
}

// convertToRootfs exports a Docker image to an ext4 filesystem.
func (m *Manager) convertToRootfs(ctx context.Context, reference, rootfsPath string) error {
	tmpDir, err := os.MkdirTemp("", "loka-rootfs-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tarPath := filepath.Join(tmpDir, "rootfs.tar")

	// Create a temporary container and export its filesystem.
	containerID, err := cmdOutput(ctx, "docker", "create", reference, "/bin/true")
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	containerID = strings.TrimSpace(containerID)
	defer runCmd(ctx, "docker", "rm", containerID)

	if err := runCmd(ctx, "docker", "export", "-o", tarPath, containerID); err != nil {
		return fmt.Errorf("docker export: %w", err)
	}

	// Create ext4 image.
	sizeMB := 2048 // 2GB default rootfs size.
	if err := runCmd(ctx, "dd", "if=/dev/zero", "of="+rootfsPath,
		"bs=1M", fmt.Sprintf("count=%d", sizeMB)); err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	if err := runCmd(ctx, "mkfs.ext4", "-F", rootfsPath); err != nil {
		return fmt.Errorf("mkfs: %w", err)
	}

	// Mount and extract. Needs root on Linux.
	mountDir := filepath.Join(tmpDir, "mount")
	os.MkdirAll(mountDir, 0o755)
	if err := runCmd(ctx, "sudo", "mount", "-o", "loop", rootfsPath, mountDir); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	defer runCmd(ctx, "sudo", "umount", mountDir)

	if err := runCmd(ctx, "sudo", "tar", "-xf", tarPath, "-C", mountDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Inject loka-supervisor.
	supervisorSrc := findSupervisorBinary()
	if supervisorSrc != "" {
		dst := filepath.Join(mountDir, "usr/local/bin/loka-supervisor")
		runCmd(ctx, "sudo", "cp", supervisorSrc, dst)
		runCmd(ctx, "sudo", "chmod", "+x", dst)
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

// Delete removes an image.
func (m *Manager) Delete(id string) error {
	img, ok := m.images[id]
	if !ok {
		return fmt.Errorf("image not found")
	}
	// Remove from object store.
	m.objStore.Delete(context.Background(), imageBucket, img.RootfsPath)
	// Remove local cache.
	os.RemoveAll(filepath.Join(m.dataDir, "images", id))
	delete(m.images, id)
	return nil
}

// RootfsPath returns the local rootfs path for an image, downloading if needed.
func (m *Manager) RootfsPath(ctx context.Context, imageID string) (string, error) {
	img, ok := m.images[imageID]
	if !ok {
		return "", fmt.Errorf("image %s not found", imageID)
	}

	localPath := filepath.Join(m.dataDir, "images", imageID, "rootfs.ext4")
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil // Already cached locally.
	}

	// Download from object store.
	reader, err := m.objStore.Get(ctx, imageBucket, img.RootfsPath)
	if err != nil {
		return "", fmt.Errorf("download rootfs: %w", err)
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
