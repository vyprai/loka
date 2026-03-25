package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
)

// BlockMountBuilder creates ext4 images for block-mode volume mounts and
// handles periodic synchronization between the ext4 image and the object store.
type BlockMountBuilder struct {
	objStore objstore.ObjectStore
	dataDir  string
	logger   *slog.Logger
}

// NewBlockMountBuilder creates a new block mount builder.
func NewBlockMountBuilder(store objstore.ObjectStore, dataDir string, logger *slog.Logger) *BlockMountBuilder {
	return &BlockMountBuilder{
		objStore: store,
		dataDir:  dataDir,
		logger:   logger,
	}
}

// BlockMountResult contains the created ext4 image path and a sync stopper.
type BlockMountResult struct {
	ImagePath string         // Path to the ext4 image on the host.
	Drive     vm.MountDrive  // Drive config for Firecracker.
	StopSync  func()         // Call to stop periodic sync.
}

// BuildImage creates a sparse ext4 image, populates it with files from the
// object store, and returns the image path. The image can be attached as a
// Firecracker drive.
func (b *BlockMountBuilder) BuildImage(ctx context.Context, serviceID string, mount loka.Volume) (*BlockMountResult, error) {
	// Create a directory for this service's block mounts.
	mountDir := filepath.Join(b.dataDir, "vms", serviceID, "mounts")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return nil, fmt.Errorf("create mount dir: %w", err)
	}

	// Generate a unique filename based on the mount path.
	safeName := strings.ReplaceAll(strings.TrimPrefix(mount.Path, "/"), "/", "_")
	if safeName == "" {
		safeName = "mount"
	}
	imagePath := filepath.Join(mountDir, safeName+".ext4")

	// Create a sparse ext4 image (1GB default, sparse so it uses minimal disk).
	if out, err := exec.Command("truncate", "-s", "1G", imagePath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create sparse image: %w (%s)", err, string(out))
	}
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", imagePath).CombinedOutput(); err != nil {
		os.Remove(imagePath)
		return nil, fmt.Errorf("mkfs.ext4: %w (%s)", err, string(out))
	}

	// Populate the image with files from the object store.
	if mount.Bucket != "" && b.objStore != nil {
		if err := b.populateImage(ctx, imagePath, mount); err != nil {
			b.logger.Warn("failed to populate block image — mount will be empty",
				"path", mount.Path, "error", err)
		}
	}

	readOnly := mount.IsReadOnly()
	result := &BlockMountResult{
		ImagePath: imagePath,
		Drive: vm.MountDrive{
			MountPath: mount.Path,
			HostPath:  imagePath,
			ReadOnly:  readOnly,
		},
	}

	// Start periodic sync for read-write mounts.
	if !readOnly {
		stopCh := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.syncLoop(ctx, imagePath, mount, stopCh)
		}()
		result.StopSync = func() {
			close(stopCh)
			wg.Wait()
		}
	} else {
		result.StopSync = func() {}
	}

	return result, nil
}

// populateImage mounts the ext4 image, downloads files from the object store
// into it, then unmounts.
func (b *BlockMountBuilder) populateImage(ctx context.Context, imagePath string, mount loka.Volume) error {
	// Create a temporary mount point.
	tmpMount, err := os.MkdirTemp("", "loka-blockmount-*")
	if err != nil {
		return fmt.Errorf("create temp mount: %w", err)
	}
	defer os.RemoveAll(tmpMount)

	// Mount the ext4 image via loop device.
	if out, err := exec.Command("mount", "-o", "loop", imagePath, tmpMount).CombinedOutput(); err != nil {
		return fmt.Errorf("mount image: %w (%s)", err, string(out))
	}
	defer exec.Command("umount", tmpMount).Run()

	// List and download objects.
	prefix := mount.Prefix
	objects, err := b.objStore.List(ctx, mount.Bucket, prefix)
	if err != nil {
		return fmt.Errorf("list objects: %w", err)
	}

	for _, obj := range objects {
		rel := strings.TrimPrefix(obj.Key, prefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}

		localPath := filepath.Join(tmpMount, rel)
		dir := filepath.Dir(localPath)
		os.MkdirAll(dir, 0o755)

		reader, err := b.objStore.Get(ctx, mount.Bucket, obj.Key)
		if err != nil {
			b.logger.Warn("skip file download", "key", obj.Key, "error", err)
			continue
		}

		f, err := os.Create(localPath)
		if err != nil {
			reader.Close()
			continue
		}
		io.Copy(f, reader)
		f.Close()
		reader.Close()
	}

	return nil
}

// syncLoop periodically syncs the ext4 image contents with the object store.
func (b *BlockMountBuilder) syncLoop(ctx context.Context, imagePath string, mount loka.Volume, stopCh chan struct{}) {
	interval := 30 * time.Second // Default sync interval for block mode.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.syncOnce(ctx, imagePath, mount); err != nil {
				b.logger.Warn("block mount sync failed",
					"path", mount.Path, "error", err)
			}
		}
	}
}

// syncOnce mounts the ext4 image, compares files with the object store,
// and uploads changed files.
func (b *BlockMountBuilder) syncOnce(ctx context.Context, imagePath string, mount loka.Volume) error {
	tmpMount, err := os.MkdirTemp("", "loka-blocksync-*")
	if err != nil {
		return fmt.Errorf("create temp mount: %w", err)
	}
	defer os.RemoveAll(tmpMount)

	// Mount read-only to avoid interfering with the VM's writes.
	if out, err := exec.Command("mount", "-o", "loop,ro", imagePath, tmpMount).CombinedOutput(); err != nil {
		return fmt.Errorf("mount image for sync: %w (%s)", err, string(out))
	}
	defer exec.Command("umount", tmpMount).Run()

	// Walk the mounted image and upload files that differ from the store.
	return filepath.Walk(tmpMount, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		rel, _ := filepath.Rel(tmpMount, path)
		key := rel
		if mount.Prefix != "" {
			key = mount.Prefix + "/" + rel
		}

		// Read the local file.
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Upload to object store.
		reader := strings.NewReader(string(data))
		if err := b.objStore.Put(ctx, mount.Bucket, key, reader, int64(len(data))); err != nil {
			b.logger.Warn("sync upload failed", "key", key, "error", err)
		}
		return nil
	})
}
