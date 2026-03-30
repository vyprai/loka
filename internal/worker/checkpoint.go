package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

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
// Uses a temp file instead of buffering the full tar.gz in memory.
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

	// 2. Tar the layer to a temp file (avoids buffering entire archive in memory).
	tmpFile, err := os.CreateTemp("", "loka-checkpoint-*.tar.gz")
	if err != nil {
		result.Error = fmt.Sprintf("create temp file: %v", err)
		return result
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := m.overlay.TarLayer(sessionID, layerName, tmpFile); err != nil {
		tmpFile.Close()
		result.Error = fmt.Sprintf("tar layer: %v", err)
		return result
	}

	// Get size and rewind for upload.
	size, _ := tmpFile.Seek(0, io.SeekEnd)
	tmpFile.Seek(0, io.SeekStart)

	// 3. Upload to object store from file (streaming, no heap spike).
	overlayKey := fmt.Sprintf("sessions/%s/checkpoints/%s/overlay.tar.gz", sessionID, checkpointID)
	if err := m.objStore.Put(ctx, checkpointBucket, overlayKey, tmpFile, size); err != nil {
		tmpFile.Close()
		result.Error = fmt.Sprintf("upload overlay: %v", err)
		return result
	}
	tmpFile.Close()
	result.OverlayKey = overlayKey

	m.logger.Info("checkpoint created",
		"checkpoint", checkpointID,
		"session", sessionID,
		"type", cpType,
		"layer", layerName,
		"size", size,
	)

	result.Success = true
	return result
}

// Restore restores a session's workspace to a checkpoint state.
// Streams directly from objstore to untar — no intermediate buffer.
func (m *CheckpointManager) Restore(ctx context.Context, sessionID, checkpointID, overlayKey string) error {
	// 1. Download overlay from object store (streaming reader).
	reader, err := m.objStore.Get(ctx, checkpointBucket, overlayKey)
	if err != nil {
		return fmt.Errorf("download overlay: %w", err)
	}
	defer reader.Close()

	// 2. Stream directly into a new layer (no memory buffering).
	layerName := fmt.Sprintf("restore-%s", checkpointID[:8])
	if err := m.overlay.UntarLayer(sessionID, layerName, reader); err != nil {
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
