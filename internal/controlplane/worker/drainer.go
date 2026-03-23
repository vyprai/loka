package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// MigrateFunc is a callback to migrate a session to another worker.
type MigrateFunc func(ctx context.Context, sessionID, targetWorkerID string) error

// Drainer gracefully drains a worker by migrating its sessions.
type Drainer struct {
	registry    *Registry
	store       store.Store
	migrateFunc MigrateFunc
	logger      *slog.Logger
}

// NewDrainer creates a new worker drainer.
func NewDrainer(registry *Registry, s store.Store, migrateFn MigrateFunc, logger *slog.Logger) *Drainer {
	return &Drainer{
		registry:    registry,
		store:       s,
		migrateFunc: migrateFn,
		logger:      logger,
	}
}

// Drain initiates a graceful drain of a worker.
// It migrates all sessions to other workers and marks the worker as draining.
func (d *Drainer) Drain(ctx context.Context, workerID string, timeout time.Duration) error {
	// Mark worker as draining.
	w, err := d.store.Workers().Get(ctx, workerID)
	if err != nil {
		return fmt.Errorf("get worker: %w", err)
	}
	w.Status = loka.WorkerStatusDraining
	w.UpdatedAt = time.Now()
	if err := d.store.Workers().Update(ctx, w); err != nil {
		return fmt.Errorf("update worker status: %w", err)
	}

	// Update registry.
	if conn, ok := d.registry.Get(workerID); ok {
		conn.Worker.Status = loka.WorkerStatusDraining
	}

	// Send drain command to worker.
	d.registry.SendCommand(workerID, WorkerCommand{
		ID:   "drain",
		Type: "drain",
		Data: map[string]int{"timeout_seconds": int(timeout.Seconds())},
	})

	d.logger.Info("worker drain started", "worker", workerID)

	// Find all sessions on this worker and migrate them.
	sessions, err := d.store.Sessions().ListByWorker(ctx, workerID)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	var migrated, failed int
	for _, sess := range sessions {
		if sess.Status == loka.SessionStatusTerminated {
			continue
		}

		// Pick a target worker (any worker except the one being drained).
		target, err := d.pickAlternateWorker(workerID)
		if err != nil {
			d.logger.Warn("no target for migration, stopping session",
				"session", sess.ID, "error", err)
			failed++
			continue
		}

		if err := d.migrateFunc(ctx, sess.ID, target.Worker.ID); err != nil {
			d.logger.Error("migration failed", "session", sess.ID, "error", err)
			failed++
		} else {
			migrated++
		}
	}

	d.logger.Info("worker drain complete",
		"worker", workerID,
		"migrated", migrated,
		"failed", failed,
	)

	return nil
}

// Undrain marks a worker as ready again.
func (d *Drainer) Undrain(ctx context.Context, workerID string) error {
	w, err := d.store.Workers().Get(ctx, workerID)
	if err != nil {
		return err
	}
	if w.Status != loka.WorkerStatusDraining {
		return fmt.Errorf("worker is not draining (status: %s)", w.Status)
	}
	w.Status = loka.WorkerStatusReady
	w.UpdatedAt = time.Now()
	if err := d.store.Workers().Update(ctx, w); err != nil {
		return err
	}
	if conn, ok := d.registry.Get(workerID); ok {
		conn.Worker.Status = loka.WorkerStatusReady
	}
	d.logger.Info("worker undrained", "worker", workerID)
	return nil
}

func (d *Drainer) pickAlternateWorker(excludeID string) (*WorkerConn, error) {
	workers := d.registry.List()
	for _, w := range workers {
		if w.Worker.ID != excludeID && w.Worker.Status == loka.WorkerStatusReady {
			return w, nil
		}
	}
	return nil, fmt.Errorf("no alternate workers available")
}
