package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/recorder"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/metrics"
	"github.com/vyprai/loka/internal/store"
)

// VolumeFailoverFunc is called when a worker dies to handle volume failover.
type VolumeFailoverFunc func(ctx context.Context, deadWorkerID string) error

// ServiceCleanupFunc is called when a worker dies to stop orphaned services.
type ServiceCleanupFunc func(ctx context.Context, deadWorkerID string) (int, error)

// Monitor watches worker health and detects failures.
type Monitor struct {
	registry           *Registry
	store              store.Store
	migrateFunc        MigrateFunc
	volumeFailoverFunc VolumeFailoverFunc
	serviceCleanupFunc ServiceCleanupFunc
	logger             *slog.Logger
	recorder           recorder.Recorder
	suspectAfter       time.Duration
	deadAfter          time.Duration
	checkInterval      time.Duration
	migrationTries     map[string]int // session ID → retry count
}

// MonitorConfig configures the health monitor.
type MonitorConfig struct {
	SuspectAfter  time.Duration // Mark suspect after no heartbeat (default 15s).
	DeadAfter     time.Duration // Mark dead after no heartbeat (default 30s).
	CheckInterval time.Duration // How often to check (default 5s).
}

// DefaultMonitorConfig returns sensible defaults.
func DefaultMonitorConfig() MonitorConfig {
	return MonitorConfig{
		SuspectAfter:  15 * time.Second,
		DeadAfter:     30 * time.Second,
		CheckInterval: 5 * time.Second,
	}
}

// NewMonitor creates a new worker health monitor.
func NewMonitor(registry *Registry, s store.Store, migrateFn MigrateFunc, cfg MonitorConfig, logger *slog.Logger, rec recorder.Recorder) *Monitor {
	if cfg.SuspectAfter == 0 {
		cfg.SuspectAfter = 15 * time.Second
	}
	if cfg.DeadAfter == 0 {
		cfg.DeadAfter = 30 * time.Second
	}
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = 5 * time.Second
	}
	if rec == nil {
		rec = recorder.NopRecorder{}
	}
	return &Monitor{
		registry:       registry,
		store:          s,
		migrateFunc:    migrateFn,
		logger:         logger,
		recorder:       rec,
		suspectAfter:   cfg.SuspectAfter,
		deadAfter:      cfg.DeadAfter,
		checkInterval:  cfg.CheckInterval,
		migrationTries: make(map[string]int),
	}
}

// SetVolumeFailoverFunc sets the function called on worker death to handle volume failover.
func (m *Monitor) SetVolumeFailoverFunc(fn VolumeFailoverFunc) {
	m.volumeFailoverFunc = fn
}

// SetServiceCleanupFunc sets the function called on worker death to stop orphaned services.
func (m *Monitor) SetServiceCleanupFunc(fn ServiceCleanupFunc) {
	m.serviceCleanupFunc = fn
}

// Start begins the health monitoring loop. Runs until ctx is canceled.
// A grace period of 30s is applied on startup to avoid false positives
// when taking over leadership in HA mode (stale heartbeat data).
func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	startedAt := time.Now()
	gracePeriod := 30 * time.Second

	m.logger.Info("worker health monitor started",
		"suspect_after", m.suspectAfter,
		"dead_after", m.deadAfter,
		"check_interval", m.checkInterval,
		"grace_period", gracePeriod,
	)

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("worker health monitor stopped")
			return
		case <-ticker.C:
			// Skip death detection during grace period (stale heartbeat data after leader failover).
			if time.Since(startedAt) < gracePeriod {
				continue
			}
			m.check(ctx)
		}
	}
}

func (m *Monitor) check(ctx context.Context) {
	now := time.Now()
	workers := m.registry.List()

	for _, conn := range workers {
		w := conn.Worker
		if w.Status == loka.WorkerStatusDraining || w.Status == loka.WorkerStatusDead {
			continue
		}

		sinceLastSeen := now.Sub(w.LastSeen)

		if sinceLastSeen > m.deadAfter && w.Status != loka.WorkerStatusDead {
			m.logger.Warn("worker marked DEAD", "worker", w.ID, "last_seen", w.LastSeen)
			m.handleWorkerDead(ctx, w)
		} else if sinceLastSeen > m.suspectAfter && w.Status != loka.WorkerStatusSuspect {
			m.logger.Warn("worker marked SUSPECT", "worker", w.ID, "last_seen", w.LastSeen)
			w.Status = loka.WorkerStatusSuspect
			w.UpdatedAt = now
			m.store.Workers().Update(ctx, w)
		}
	}
}

func (m *Monitor) handleWorkerDead(ctx context.Context, w *loka.Worker) {
	w.Status = loka.WorkerStatusDead
	w.UpdatedAt = time.Now()
	m.store.Workers().Update(ctx, w)
	m.recorder.Inc("worker_deaths_total", metrics.Label{Name: "worker_id", Value: w.ID}, metrics.Label{Name: "hostname", Value: w.Hostname})

	// Find orphaned sessions and reschedule them.
	sessions, err := m.store.Sessions().ListByWorker(ctx, w.ID)
	if err != nil {
		m.logger.Error("failed to list orphaned sessions", "worker", w.ID, "error", err)
		return
	}

	rescheduled := 0
	for _, sess := range sessions {
		if sess.Status == loka.SessionStatusTerminated {
			continue
		}

		// Mark session as creating (it needs to be rescheduled).
		sess.Status = loka.SessionStatusCreating
		sess.WorkerID = ""
		sess.UpdatedAt = time.Now()
		m.store.Sessions().Update(ctx, sess)

		// Try to find a new worker and migrate.
		target := m.pickHealthyWorker(w.ID)
		if target == nil {
			m.logger.Warn("no healthy worker for rescheduling", "session", sess.ID)
			continue
		}

		if m.migrateFunc != nil {
			m.migrationTries[sess.ID]++
			if err := m.migrateFunc(ctx, sess.ID, target.Worker.ID); err != nil {
				m.logger.Error("rescheduling failed", "session", sess.ID, "attempt", m.migrationTries[sess.ID], "error", err)
				// After 3 failures, give up and mark as error.
				if m.migrationTries[sess.ID] >= 3 {
					sess.Status = loka.SessionStatusError
					sess.UpdatedAt = time.Now()
					m.store.Sessions().Update(ctx, sess)
					delete(m.migrationTries, sess.ID)
					m.logger.Warn("session migration abandoned after 3 failures", "session", sess.ID)
				}
			} else {
				delete(m.migrationTries, sess.ID)
				rescheduled++
			}
		}
	}

	// Stop orphaned services on the dead worker.
	if m.serviceCleanupFunc != nil {
		if count, err := m.serviceCleanupFunc(ctx, w.ID); err != nil {
			m.logger.Error("service cleanup failed", "worker", w.ID, "error", err)
		} else if count > 0 {
			m.logger.Info("orphaned services stopped", "worker", w.ID, "count", count)
		}
	}

	// Handle volume failover: promote replicas, assign new replicas.
	if m.volumeFailoverFunc != nil {
		if err := m.volumeFailoverFunc(ctx, w.ID); err != nil {
			m.logger.Error("volume failover failed", "worker", w.ID, "error", err)
		}
	}

	m.logger.Info("worker dead handling complete",
		"worker", w.ID,
		"orphaned_sessions", len(sessions),
		"rescheduled", rescheduled,
	)
}

func (m *Monitor) pickHealthyWorker(excludeID string) *WorkerConn {
	workers := m.registry.List()
	for _, w := range workers {
		if w.Worker.ID != excludeID &&
			(w.Worker.Status == loka.WorkerStatusReady || w.Worker.Status == loka.WorkerStatusBusy) {
			return w
		}
	}
	return nil
}
