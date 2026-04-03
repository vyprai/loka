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

	"github.com/vyprai/loka/internal/controlplane/metrics/recorder"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/metrics"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
)

// WorkerRegistry provides worker lookup for volume placement.
type WorkerRegistry interface {
	// ListHealthy returns IDs of all healthy workers.
	ListHealthy(ctx context.Context) ([]string, error)
}

// Manager handles named volume lifecycle in the control plane.
// The manager is METADATA ONLY — no volume data is stored on the control plane.
// Actual data lives on workers (block volumes) or objstore (object volumes).
type Manager struct {
	store    store.Store
	objStore objstore.ObjectStore // may be nil (block-only mode)
	workers  WorkerRegistry
	logger   *slog.Logger
	recorder recorder.Recorder
}

// NewManager creates a new volume manager.
func NewManager(s store.Store, objStore objstore.ObjectStore, workers WorkerRegistry, logger *slog.Logger, rec recorder.Recorder) *Manager {
	if rec == nil {
		rec = recorder.NopRecorder{}
	}
	return &Manager{store: s, objStore: objStore, workers: workers, logger: logger, recorder: rec}
}

// CreateBlock creates a new block volume record. Block volumes are folders on
// worker hosts, replicated across workers. No data is stored on the CP.
func (m *Manager) CreateBlock(ctx context.Context, name string, maxSizeBytes int64) (*loka.VolumeRecord, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	if existing, err := m.store.Volumes().Get(ctx, name); err == nil && existing != nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}

	now := time.Now()
	vol := &loka.VolumeRecord{
		Name:            name,
		Type:            loka.VolumeTypeBlock,
		Status:          loka.VolumeStatusDegraded, // No workers assigned yet.
		Provider:        "volume",
		MaxSizeBytes:    maxSizeBytes,
		DesiredReplicas: 2,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := m.store.Volumes().Create(ctx, vol); err != nil {
		return nil, fmt.Errorf("create volume: %w", err)
	}

	m.recorder.Inc("volumes_created_total", metrics.Label{Name: "name", Value: name}, metrics.Label{Name: "type", Value: "block"})
	m.logger.Info("block volume created", "name", name, "max_size", maxSizeBytes)
	return vol, nil
}

// CreateObject creates an object volume. Two sub-modes:
//   - Direct connection (bucket != ""): user provides their own bucket/credentials.
//   - Loka-managed (bucket == ""): uses default objstore if available, else falls back to block.
func (m *Manager) CreateObject(ctx context.Context, name, bucket, prefix, region, creds string, maxSizeBytes int64) (*loka.VolumeRecord, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	if existing, err := m.store.Volumes().Get(ctx, name); err == nil && existing != nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}

	now := time.Now()

	if bucket != "" {
		// Direct connection — user provides their own bucket.
		vol := &loka.VolumeRecord{
			Name:         name,
			Type:         loka.VolumeTypeObject,
			Status:       loka.VolumeStatusHealthy,
			Provider:     inferProvider(bucket, region),
			Bucket:       bucket,
			Prefix:       prefix,
			Region:       region,
			Credentials:  creds,
			MaxSizeBytes: maxSizeBytes,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := m.store.Volumes().Create(ctx, vol); err != nil {
			return nil, fmt.Errorf("create volume: %w", err)
		}
		m.recorder.Inc("volumes_created_total", metrics.Label{Name: "name", Value: name}, metrics.Label{Name: "type", Value: "object_direct"})
		m.logger.Info("direct object volume created", "name", name, "bucket", bucket)
		return vol, nil
	}

	// Loka-managed object volume.
	if m.objStore != nil {
		// Use default Loka objstore.
		vol := &loka.VolumeRecord{
			Name:         name,
			Type:         loka.VolumeTypeObject,
			Status:       loka.VolumeStatusHealthy,
			Provider:     "volume",
			Bucket:       "volumes",
			Prefix:       name + "/",
			MaxSizeBytes: maxSizeBytes,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := m.store.Volumes().Create(ctx, vol); err != nil {
			return nil, fmt.Errorf("create volume: %w", err)
		}
		m.recorder.Inc("volumes_created_total", metrics.Label{Name: "name", Value: name}, metrics.Label{Name: "type", Value: "object_managed"})
		m.logger.Info("loka-managed object volume created", "name", name)
		return vol, nil
	}

	// No objstore — fall back to block volume behavior.
	m.logger.Info("no objstore configured, falling back to block volume", "name", name)
	return m.CreateBlock(ctx, name, maxSizeBytes)
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

	// For Loka-managed object volumes, clean up objstore.
	if vol.Type == loka.VolumeTypeObject && !vol.IsDirectObject() && m.objStore != nil {
		objects, err := m.objStore.List(ctx, vol.Bucket, vol.Prefix)
		if err == nil {
			for _, obj := range objects {
				_ = m.objStore.Delete(ctx, vol.Bucket, obj.Key)
			}
		}
	}

	if err := m.store.Volumes().Delete(ctx, name); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}

	m.logger.Info("volume deleted", "name", name)
	return nil
}

// IncrementMountCount atomically increments the mount count for a volume.
// If the volume record does not exist, it is auto-created as a block volume.
func (m *Manager) IncrementMountCount(ctx context.Context, name string) error {
	// Check if volume exists.
	if _, err := m.store.Volumes().Get(ctx, name); err != nil {
		// Volume doesn't exist — auto-create as block volume with count=1.
		now := time.Now()
		vol := &loka.VolumeRecord{
			Name:            name,
			Type:            loka.VolumeTypeBlock,
			Status:          loka.VolumeStatusDegraded,
			Provider:        "volume",
			MountCount:      1,
			DesiredReplicas: 2,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		return m.store.Volumes().Create(ctx, vol)
	}
	return m.store.Volumes().IncrementMountCount(ctx, name)
}

// DecrementMountCount atomically decrements the mount count for a volume (clamped at 0).
func (m *Manager) DecrementMountCount(ctx context.Context, name string) error {
	if _, err := m.store.Volumes().Get(ctx, name); err != nil {
		return fmt.Errorf("volume %q not found: %w", name, err)
	}
	return m.store.Volumes().DecrementMountCount(ctx, name)
}

// AssignPrimary assigns a worker as the primary holder for a block volume.
func (m *Manager) AssignPrimary(ctx context.Context, name, workerID string) error {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		return fmt.Errorf("volume %q not found: %w", name, err)
	}
	if !vol.IsLokaManaged() {
		return nil // Direct object volumes don't have placement.
	}

	vol.PrimaryWorkerID = workerID
	vol.UpdatedAt = time.Now()

	// Determine status.
	if len(vol.ReplicaWorkerIDs) > 0 {
		vol.Status = loka.VolumeStatusHealthy
	} else {
		vol.Status = loka.VolumeStatusDegraded
	}

	return m.store.Volumes().Update(ctx, vol)
}

// AssignReplica picks a healthy worker (different from primary) as a replica.
func (m *Manager) AssignReplica(ctx context.Context, name string) error {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		return fmt.Errorf("volume %q not found: %w", name, err)
	}
	if !vol.IsLokaManaged() {
		return nil // Direct object volumes don't need replicas.
	}

	if m.workers == nil {
		return fmt.Errorf("worker registry not available")
	}

	healthy, err := m.workers.ListHealthy(ctx)
	if err != nil {
		return fmt.Errorf("list healthy workers: %w", err)
	}

	// Find a worker that is not the primary and not already a replica.
	existing := make(map[string]bool)
	existing[vol.PrimaryWorkerID] = true
	for _, id := range vol.ReplicaWorkerIDs {
		existing[id] = true
	}

	var replicaWorkerID string
	for _, id := range healthy {
		if !existing[id] {
			replicaWorkerID = id
			break
		}
	}

	if replicaWorkerID == "" {
		// Single worker mode — no replica available.
		m.logger.Warn("no replica worker available, volume degraded", "volume", name)
		return m.store.Volumes().UpdateStatus(ctx, name, loka.VolumeStatusDegraded)
	}

	vol.ReplicaWorkerIDs = append(vol.ReplicaWorkerIDs, replicaWorkerID)
	vol.Status = loka.VolumeStatusSyncing
	vol.UpdatedAt = time.Now()

	if err := m.store.Volumes().Update(ctx, vol); err != nil {
		return err
	}

	m.logger.Info("replica assigned", "volume", name, "replica", replicaWorkerID)
	return nil
}

// HandleWorkerDeath handles volume failover when a worker dies.
func (m *Manager) HandleWorkerDeath(ctx context.Context, deadWorkerID string) error {
	vols, err := m.store.Volumes().ListByWorker(ctx, deadWorkerID)
	if err != nil {
		return fmt.Errorf("list volumes for dead worker: %w", err)
	}

	for _, vol := range vols {
		if !vol.IsLokaManaged() {
			continue
		}

		m.recorder.Inc("volume_failovers_total", metrics.Label{Name: "volume", Value: vol.Name}, metrics.Label{Name: "dead_worker", Value: deadWorkerID})

		if vol.PrimaryWorkerID == deadWorkerID {
			// Promote first replica to primary.
			if len(vol.ReplicaWorkerIDs) > 0 {
				newPrimary := vol.ReplicaWorkerIDs[0]
				remaining := vol.ReplicaWorkerIDs[1:]
				if err := m.store.Volumes().UpdatePlacement(ctx, vol.Name, newPrimary, remaining); err != nil {
					m.logger.Error("failed to promote replica", "volume", vol.Name, "error", err)
					continue
				}
				m.logger.Info("volume primary promoted", "volume", vol.Name, "new_primary", newPrimary)

				// Mark degraded and try to assign new replica.
				m.store.Volumes().UpdateStatus(ctx, vol.Name, loka.VolumeStatusDegraded)
				if err := m.AssignReplica(ctx, vol.Name); err != nil {
					m.logger.Warn("failed to assign new replica after failover", "volume", vol.Name, "error", err)
				}
			} else {
				// No replicas — data is lost.
				m.store.Volumes().UpdatePlacement(ctx, vol.Name, "", nil)
				m.store.Volumes().UpdateStatus(ctx, vol.Name, loka.VolumeStatusError)
				m.logger.Error("volume data lost, no replicas", "volume", vol.Name)
			}
		} else {
			// Dead worker was a replica — remove it, assign new one.
			var remaining []string
			for _, id := range vol.ReplicaWorkerIDs {
				if id != deadWorkerID {
					remaining = append(remaining, id)
				}
			}
			m.store.Volumes().UpdatePlacement(ctx, vol.Name, vol.PrimaryWorkerID, remaining)
			m.store.Volumes().UpdateStatus(ctx, vol.Name, loka.VolumeStatusDegraded)
			if err := m.AssignReplica(ctx, vol.Name); err != nil {
				m.logger.Warn("failed to assign new replica", "volume", vol.Name, "error", err)
			}
		}
	}
	return nil
}

// ReconcileDegradedVolumes checks for degraded volumes and tries to assign replicas.
// Called when a new worker joins.
func (m *Manager) ReconcileDegradedVolumes(ctx context.Context) error {
	vols, err := m.store.Volumes().List(ctx)
	if err != nil {
		return err
	}
	for _, vol := range vols {
		if vol.Status == loka.VolumeStatusDegraded && vol.IsLokaManaged() && vol.PrimaryWorkerID != "" {
			if len(vol.ReplicaWorkerIDs) < vol.DesiredReplicas-1 {
				if err := m.AssignReplica(ctx, vol.Name); err != nil {
					m.logger.Warn("reconcile replica failed", "volume", vol.Name, "error", err)
				}
			}
		}
	}
	return nil
}

// UpdateSizeReport updates the current size of a volume (reported by the primary worker).
func (m *Manager) UpdateSizeReport(ctx context.Context, name string, sizeBytes int64) error {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		return err
	}
	vol.SizeBytes = sizeBytes
	vol.UpdatedAt = time.Now()
	return m.store.Volumes().Update(ctx, vol)
}

// ListFiles lists files for a Loka-managed object volume via objstore.
func (m *Manager) ListFiles(ctx context.Context, name string) ([]objstore.ObjectInfo, error) {
	vol, err := m.store.Volumes().Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if vol.Type == loka.VolumeTypeObject && vol.Bucket != "" && m.objStore != nil {
		return m.objStore.List(ctx, vol.Bucket, vol.Prefix)
	}
	// Block volumes: files are on workers, not on CP. Return empty.
	return nil, nil
}

// BundlePath returns the local directory path for an extracted bundle (readonly).
// Bundles are service code snapshots, not user volumes — they live on the CP
// temporarily until sent to workers.
func (m *Manager) BundlePath(name string) string {
	return filepath.Join(os.TempDir(), "loka-bundles", name)
}

// ExtractBundle downloads a bundle tar.gz from the object store and extracts
// its contents into a temporary directory for fast virtiofs access.
func (m *Manager) ExtractBundle(ctx context.Context, volName, bundleKey string) error {
	if m.objStore == nil {
		return fmt.Errorf("object store not configured")
	}

	// Skip extraction if already done.
	bundleDir := m.BundlePath(volName)
	if entries, err := os.ReadDir(bundleDir); err == nil && len(entries) > 0 {
		return nil
	}

	parts := strings.SplitN(bundleKey, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid bundle key %q: expected 'bucket/key'", bundleKey)
	}
	srcBucket, srcKey := parts[0], parts[1]

	reader, err := m.objStore.Get(ctx, srcBucket, srcKey)
	if err != nil {
		return fmt.Errorf("download bundle: %w", err)
	}
	defer reader.Close()

	gzReader, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gzReader.Close()

	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(bundleDir)
		}
	}()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		target := filepath.Join(bundleDir, filepath.Clean(header.Name))
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
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Remove(target)
			os.Symlink(header.Linkname, target)
		}
	}

	success = true
	return nil
}

// inferProvider guesses the cloud provider from bucket/region.
func inferProvider(bucket, region string) string {
	if region != "" {
		switch {
		case len(region) > 2 && region[2] == '-': // "us-east-1" pattern
			return "s3"
		case region == "auto":
			return "s3" // R2 uses "auto"
		}
	}
	return "s3" // Default to S3-compatible.
}
