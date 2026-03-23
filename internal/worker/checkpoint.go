package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
)

const checkpointBucket = "checkpoints"

// CheckpointManager handles checkpoint create/restore operations on the worker.
type CheckpointManager struct {
	overlay  *vm.OverlayManager
	objStore objstore.ObjectStore
	logger   *slog.Logger
}

// NewCheckpointManager creates a new checkpoint manager.
func NewCheckpointManager(overlay *vm.OverlayManager, objStore objstore.ObjectStore, logger *slog.Logger) *CheckpointManager {
	return &CheckpointManager{
		overlay:  overlay,
		objStore: objStore,
		logger:   logger,
	}
}

// CheckpointResult holds the result of a checkpoint operation.
type CheckpointResult struct {
	CheckpointID string
	SessionID    string
	LayerName    string
	OverlayKey   string // Object store key for the overlay tar.
	Success      bool
	Error        string
}

// Create creates a checkpoint for a session.
// For light checkpoints: snapshots the workspace overlay and uploads to objstore.
// For full checkpoints: same as light + captures additional state metadata.
func (m *CheckpointManager) Create(ctx context.Context, sessionID, checkpointID string, cpType loka.CheckpointType) *CheckpointResult {
	result := &CheckpointResult{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
	}

	// 1. Create an overlay layer from current workspace state.
	layerName, err := m.overlay.CreateLayer(sessionID)
	if err != nil {
		result.Error = fmt.Sprintf("create layer: %v", err)
		return result
	}
	result.LayerName = layerName

	// 2. Tar the layer.
	var buf bytes.Buffer
	if err := m.overlay.TarLayer(sessionID, layerName, &buf); err != nil {
		result.Error = fmt.Sprintf("tar layer: %v", err)
		return result
	}

	// 3. Upload to object store.
	overlayKey := fmt.Sprintf("sessions/%s/checkpoints/%s/overlay.tar.gz", sessionID, checkpointID)
	if err := m.objStore.Put(ctx, checkpointBucket, overlayKey, &buf, int64(buf.Len())); err != nil {
		result.Error = fmt.Sprintf("upload overlay: %v", err)
		return result
	}
	result.OverlayKey = overlayKey

	m.logger.Info("checkpoint created",
		"checkpoint", checkpointID,
		"session", sessionID,
		"type", cpType,
		"layer", layerName,
		"size", buf.Len(),
	)

	result.Success = true
	return result
}

// Restore restores a session's workspace to a checkpoint state.
func (m *CheckpointManager) Restore(ctx context.Context, sessionID, checkpointID, overlayKey string) error {
	// 1. Download overlay from object store.
	reader, err := m.objStore.Get(ctx, checkpointBucket, overlayKey)
	if err != nil {
		return fmt.Errorf("download overlay: %w", err)
	}
	defer reader.Close()

	// Read into buffer (needed for untar).
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		return fmt.Errorf("read overlay: %w", err)
	}

	// 2. Create a layer from the downloaded data.
	layerName := fmt.Sprintf("restore-%s", checkpointID[:8])
	if err := m.overlay.UntarLayer(sessionID, layerName, &buf); err != nil {
		return fmt.Errorf("untar layer: %w", err)
	}

	// 3. Restore workspace from this layer.
	if err := m.overlay.RestoreLayer(sessionID, layerName); err != nil {
		return fmt.Errorf("restore layer: %w", err)
	}

	m.logger.Info("checkpoint restored",
		"checkpoint", checkpointID,
		"session", sessionID,
		"layer", layerName,
	)

	return nil
}

// Diff returns the filesystem differences between two checkpoints.
func (m *CheckpointManager) Diff(sessionID, layerA, layerB string) ([]vm.DiffEntry, error) {
	return m.overlay.DiffLayers(sessionID, layerA, layerB)
}
