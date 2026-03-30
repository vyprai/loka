package volume

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
)

const volumeBucket = "volumes"

// Manager handles named volume lifecycle in the control plane.
// Volumes are stored as local directories at {dataDir}/volumes/{name}/ for fast
// virtiofs access. They are also backed to objstore for persistence and cross-worker sync.
type Manager struct {
	store    store.Store
	objStore objstore.ObjectStore
	dataDir  string // Root data directory (volumes live at {dataDir}/volumes/).
	logger   *slog.Logger
}

// NewManager creates a new volume manager.
func NewManager(s store.Store, objStore objstore.ObjectStore, dataDir string, logger *slog.Logger) *Manager {
	os.MkdirAll(filepath.Join(dataDir, "volumes"), 0o755)
	return &Manager{store: s, objStore: objStore, dataDir: dataDir, logger: logger}
}

// VolumePath returns the local directory path for a named volume.
func (m *Manager) VolumePath(name string) string {
	return filepath.Join(m.dataDir, "volumes", name)
}

// BundlePath returns the local directory path for a bundle (readonly).
func (m *Manager) BundlePath(name string) string {
	return filepath.Join(m.dataDir, "bundles", name)
}

// Create creates a new named volume record.
func (m *Manager) Create(ctx context.Context, name string) (*loka.VolumeRecord, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name is required")
	}

	// Check if already exists.
	if existing, err := m.store.Volumes().Get(ctx, name); err == nil && existing != nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}

	now := time.Now()
	vol := &loka.VolumeRecord{
		Name:      name,
		Provider:  "volume",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := m.store.Volumes().Create(ctx, vol); err != nil {
		return nil, fmt.Errorf("create volume: %w", err)
	}

	m.logger.Info("volume created", "name", name)
	return vol, nil
}

// Get retrieves a volume record by name.
func (m *Manager) Get(ctx context.Context, name string) (*loka.VolumeRecord, error) {
	return m.store.Volumes().Get(ctx, name)
}

// List returns all volume records.
func (m *Manager) List(ctx context.Context) ([]*loka.VolumeRecord, error) {
	return m.store.Volumes().List(ctx)
}

// Delete deletes a named volume. Fails if the volume is currently mounted.
func (m *Manager) Delete(ctx context.Context, name string) error {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		return fmt.Errorf("volume %q not found", name)
	}
	if vol.MountCount > 0 {
		return fmt.Errorf("volume %q is mounted by %d VMs, unmount first", name, vol.MountCount)
	}

	// Delete objects from objstore.
	if m.objStore != nil {
		objects, err := m.objStore.List(ctx, volumeBucket, name+"/")
		if err == nil {
			for _, obj := range objects {
				_ = m.objStore.Delete(ctx, volumeBucket, obj.Key)
			}
		}
	}

	if err := m.store.Volumes().Delete(ctx, name); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}

	m.logger.Info("volume deleted", "name", name)
	return nil
}

// IncrementMountCount increments the mount count for a volume.
// If the volume record does not exist, it is auto-created.
func (m *Manager) IncrementMountCount(ctx context.Context, name string) error {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		// Auto-create the volume record.
		now := time.Now()
		vol = &loka.VolumeRecord{
			Name:       name,
			Provider:   "volume",
			MountCount: 1,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		return m.store.Volumes().Create(ctx, vol)
	}
	vol.MountCount++
	vol.UpdatedAt = time.Now()
	return m.store.Volumes().Update(ctx, vol)
}

// DecrementMountCount decrements the mount count for a volume.
func (m *Manager) DecrementMountCount(ctx context.Context, name string) error {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		return fmt.Errorf("volume %q not found: %w", name, err)
	}
	if vol.MountCount > 0 {
		vol.MountCount--
	}
	vol.UpdatedAt = time.Now()
	return m.store.Volumes().Update(ctx, vol)
}

// ListFiles lists files stored in a named volume via objstore.
func (m *Manager) ListFiles(ctx context.Context, name string) ([]objstore.ObjectInfo, error) {
	if m.objStore == nil {
		return nil, fmt.Errorf("object store not configured")
	}
	return m.objStore.List(ctx, volumeBucket, name+"/")
}

// ExtractBundle downloads a bundle tar.gz from the object store and extracts
// its contents into a local volume directory for fast virtiofs access.
// The bundleKey format is "bucket/key" (e.g. "services/abc/bundle.tar.gz").
func (m *Manager) ExtractBundle(ctx context.Context, volName, bundleKey string) error {
	if m.objStore == nil {
		return fmt.Errorf("object store not configured")
	}

	// Skip extraction if bundle already extracted (deduplication).
	volDir := m.BundlePath(volName)
	if entries, err := os.ReadDir(volDir); err == nil && len(entries) > 0 {
		m.logger.Info("bundle already extracted, skipping", "volume", volName)
		return nil
	}

	// Parse bundleKey into bucket and key.
	parts := strings.SplitN(bundleKey, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid bundle key %q: expected 'bucket/key'", bundleKey)
	}
	srcBucket, srcKey := parts[0], parts[1]

	// Download the bundle.
	reader, err := m.objStore.Get(ctx, srcBucket, srcKey)
	if err != nil {
		return fmt.Errorf("download bundle: %w", err)
	}
	defer reader.Close()

	// Decompress gzip.
	gzReader, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gzReader.Close()

	// Extract tar entries to the readonly bundles directory.
	// Clean up partial directory on error.
	volDir = m.BundlePath(volName)
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(volDir)
		}
	}()

	tarReader := tar.NewReader(gzReader)
	fileCount := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		target := filepath.Join(volDir, filepath.Clean(header.Name))

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(header.Mode)|0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				continue
			}
			io.Copy(f, tarReader)
			f.Close()
			fileCount++
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			os.Symlink(header.Linkname, target)
			fileCount++
		}
	}

	success = true
	m.logger.Info("bundle extracted into volume",
		"volume", volName,
		"bundle_key", bundleKey,
		"files", fileCount,
		"path", volDir,
	)
	return nil
}
