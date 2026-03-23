package session

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
)

// MigrateSession migrates a session from its current worker to a target worker.
// Steps: checkpoint on source → update DB → restore on target.
func (m *Manager) MigrateSession(ctx context.Context, sessionID, targetWorkerID string) error {
	s, err := m.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if s.WorkerID == targetWorkerID {
		return fmt.Errorf("session already on worker %s", targetWorkerID)
	}

	sourceWorkerID := s.WorkerID
	m.logger.Info("migrating session",
		"session", sessionID,
		"from", sourceWorkerID,
		"to", targetWorkerID,
	)

	// 1. Create a migration checkpoint on the source worker.
	cpID := uuid.New().String()
	cp, err := m.CreateCheckpoint(ctx, sessionID, cpID, loka.CheckpointFull, "")
	if err != nil {
		return fmt.Errorf("create migration checkpoint: %w", err)
	}

	// 2. Wait for checkpoint to be ready.
	for i := 0; i < 100; i++ { // Up to 10 seconds.
		updated, err := m.store.Checkpoints().Get(ctx, cpID)
		if err == nil && updated.Status == loka.CheckpointStatusReady {
			cp = updated
			break
		}
		if err == nil && updated.Status == loka.CheckpointStatusFailed {
			return fmt.Errorf("migration checkpoint failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cp.Status != loka.CheckpointStatusReady {
		return fmt.Errorf("migration checkpoint timed out")
	}

	// 3. Stop session on source worker.
	if sourceWorkerID != "" {
		m.registry.SendCommand(sourceWorkerID, worker.WorkerCommand{
			ID:   uuid.New().String(),
			Type: "stop_session",
			Data: map[string]string{"session_id": sessionID},
		})
	}

	// 4. Update session's worker assignment.
	s.WorkerID = targetWorkerID
	s.UpdatedAt = time.Now()
	if err := m.store.Sessions().Update(ctx, s); err != nil {
		return fmt.Errorf("update session worker: %w", err)
	}

	// 5. Restore checkpoint on target worker — first launch session, then restore.
	m.registry.SendCommand(targetWorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "launch_session",
		Data: worker.LaunchSessionData{
			SessionID:  sessionID,
			ImageRef:   s.ImageRef,
			Mode:       s.Mode,
			ExecPolicy: s.ExecPolicy,
			VCPUs:      s.VCPUs,
			MemoryMB:   s.MemoryMB,
		},
	})

	// Small delay to let session initialize on target.
	time.Sleep(200 * time.Millisecond)

	m.registry.SendCommand(targetWorkerID, worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "restore_checkpoint",
		Data: worker.RestoreCheckpointData{
			SessionID:    sessionID,
			CheckpointID: cpID,
			OverlayKey:   cp.OverlayPath,
		},
	})

	m.logger.Info("session migration complete",
		"session", sessionID,
		"from", sourceWorkerID,
		"to", targetWorkerID,
	)

	return nil
}
