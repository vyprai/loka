package gc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/config"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
)

// GarbageCollector periodically removes expired sessions, executions,
// tokens, checkpoints, and unused images.
type GarbageCollector struct {
	store    store.Store
	objStore objstore.ObjectStore
	registry *worker.Registry
	images   *image.Manager
	retention config.RetentionConfig
	logger   *slog.Logger

	mu             sync.Mutex
	pendingCleanup map[string][]string // workerID → []sessionID for retry
	cleanupRetries map[string]int      // "workerID:sessionID" → retry count
	lastRun        time.Time
	lastResult     *SweepResult
}

// SweepResult summarises one garbage collection pass.
type SweepResult struct {
	SessionsPurged     int
	ExecutionsPurged   int
	TokensPurged       int
	CheckpointsCleaned int
	WorkersCleaned     int
	ImagesCleaned      int
	Errors             []string
	Duration           time.Duration
	Timestamp          time.Time
}

// New creates a GarbageCollector.
func New(
	s store.Store,
	obj objstore.ObjectStore,
	reg *worker.Registry,
	img *image.Manager,
	ret config.RetentionConfig,
	logger *slog.Logger,
) *GarbageCollector {
	return &GarbageCollector{
		store:          s,
		objStore:       obj,
		registry:       reg,
		images:         img,
		retention:      ret,
		logger:         logger,
		pendingCleanup: make(map[string][]string),
		cleanupRetries: make(map[string]int),
	}
}

// Run starts the GC loop. It blocks until ctx is cancelled.
// Should be called inside coordinator.ElectLeader for HA so that only
// the leader node runs GC.
func (gc *GarbageCollector) Run(ctx context.Context) {
	interval, err := time.ParseDuration(gc.retention.CleanupInterval)
	if err != nil {
		gc.logger.Error("invalid cleanup_interval, using 1h", "err", err)
		interval = time.Hour
	}

	gc.logger.Info("garbage collector started", "interval", interval)

	// Run an initial sweep immediately.
	gc.Sweep(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			gc.logger.Info("garbage collector stopped")
			return
		case <-ticker.C:
			gc.Sweep(ctx)
		}
	}
}

// Sweep performs one garbage collection pass.
func (gc *GarbageCollector) Sweep(ctx context.Context) *SweepResult {
	// Add timeout to prevent hanging on slow store operations.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	start := time.Now()
	result := &SweepResult{Timestamp: start}

	gc.logger.Info("gc sweep starting")

	// Parse TTLs.
	sessionTTL := parseDurationOrDefault(gc.retention.SessionTTL, 168*time.Hour)
	executionTTL := parseDurationOrDefault(gc.retention.ExecutionTTL, 72*time.Hour)
	tokenTTL := parseDurationOrDefault(gc.retention.TokenTTL, 24*time.Hour)
	imageTTL := parseDurationOrDefault(gc.retention.ImageTTL, 720*time.Hour)

	// 1. Purge terminated sessions.
	if err := ctx.Err(); err != nil {
		result.Duration = time.Since(start)
		gc.saveResult(result)
		return result
	}
	n, err := gc.store.Sessions().DeleteTerminatedBefore(ctx, time.Now().Add(-sessionTTL))
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("sessions: %v", err))
	} else {
		result.SessionsPurged = n
	}

	// 2. Purge completed executions.
	if err := ctx.Err(); err != nil {
		result.Duration = time.Since(start)
		gc.saveResult(result)
		return result
	}
	n, err = gc.store.Executions().DeleteCompletedBefore(ctx, time.Now().Add(-executionTTL))
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("executions: %v", err))
	} else {
		result.ExecutionsPurged = n
	}

	// 3. Purge expired tokens.
	if err := ctx.Err(); err != nil {
		result.Duration = time.Since(start)
		gc.saveResult(result)
		return result
	}
	n, err = gc.store.Tokens().DeleteExpiredBefore(ctx, time.Now().Add(-tokenTTL))
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("tokens: %v", err))
	} else {
		result.TokensPurged = n
	}

	// 4. Clean up workers: send cleanup commands for sessions that no longer exist.
	if err := ctx.Err(); err != nil {
		result.Duration = time.Since(start)
		gc.saveResult(result)
		return result
	}
	workersCleaned := gc.cleanupWorkers(ctx)
	result.WorkersCleaned = workersCleaned

	// 5. Clean up stale images past their TTL.
	if err := ctx.Err(); err != nil {
		result.Duration = time.Since(start)
		gc.saveResult(result)
		return result
	}
	imagesCleaned := gc.cleanupImages(imageTTL)
	result.ImagesCleaned = imagesCleaned

	result.Duration = time.Since(start)
	gc.saveResult(result)

	gc.logger.Info("gc sweep complete",
		"sessions", result.SessionsPurged,
		"executions", result.ExecutionsPurged,
		"tokens", result.TokensPurged,
		"workers", result.WorkersCleaned,
		"images", result.ImagesCleaned,
		"errors", len(result.Errors),
		"duration", result.Duration,
	)

	return result
}

// SweepDryRun returns what would be cleaned without actually cleaning.
func (gc *GarbageCollector) SweepDryRun(ctx context.Context) *SweepResult {
	start := time.Now()
	result := &SweepResult{Timestamp: start}

	sessionTTL := parseDurationOrDefault(gc.retention.SessionTTL, 168*time.Hour)
	executionTTL := parseDurationOrDefault(gc.retention.ExecutionTTL, 72*time.Hour)
	imageTTL := parseDurationOrDefault(gc.retention.ImageTTL, 720*time.Hour)

	// Count terminated sessions past TTL.
	cutoff := time.Now().Add(-sessionTTL)
	sessions, err := gc.store.Sessions().List(ctx, store.SessionFilter{})
	if err == nil {
		for _, s := range sessions {
			if s.Status == "terminated" && s.UpdatedAt.Before(cutoff) {
				result.SessionsPurged++
			}
		}
	}

	// Count completed executions past TTL.
	// We cannot easily count without a dedicated query, so we approximate
	// by noting the dry-run is advisory.
	_ = executionTTL

	// Count stale images.
	imgCutoff := time.Now().Add(-imageTTL)
	for _, img := range gc.images.List() {
		if img.CreatedAt.Before(imgCutoff) {
			result.ImagesCleaned++
		}
	}

	// Count pending worker cleanups.
	gc.mu.Lock()
	for _, sids := range gc.pendingCleanup {
		result.WorkersCleaned += len(sids)
	}
	gc.mu.Unlock()

	result.Duration = time.Since(start)
	return result
}

// LastResult returns the result of the most recent sweep.
func (gc *GarbageCollector) LastResult() *SweepResult {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.lastResult
}

// ── internal helpers ────────────────────────────────────────

func (gc *GarbageCollector) saveResult(r *SweepResult) {
	gc.mu.Lock()
	gc.lastRun = r.Timestamp
	gc.lastResult = r
	gc.mu.Unlock()
}

// cleanupWorkers iterates connected workers and sends cleanup commands
// for sessions that no longer exist in the store. Offline workers are
// tracked in pendingCleanup for retry on the next sweep.
func (gc *GarbageCollector) cleanupWorkers(ctx context.Context) int {
	cleaned := 0

	// First, retry any pending cleanups from previous sweeps.
	gc.mu.Lock()
	pending := gc.pendingCleanup
	gc.pendingCleanup = make(map[string][]string)
	gc.mu.Unlock()

	for workerID, sessionIDs := range pending {
		for _, sid := range sessionIDs {
			retryKey := workerID + ":" + sid
			if err := gc.sendCleanup(workerID, sid); err != nil {
				gc.mu.Lock()
				gc.cleanupRetries[retryKey]++
				if gc.cleanupRetries[retryKey] <= 3 {
					gc.pendingCleanup[workerID] = append(gc.pendingCleanup[workerID], sid)
				} else {
					// Give up after 3 retries — worker is likely permanently gone.
					delete(gc.cleanupRetries, retryKey)
					gc.logger.Warn("gc: abandoned cleanup after 3 retries", "worker", workerID, "session", sid)
				}
				gc.mu.Unlock()
			} else {
				gc.mu.Lock()
				delete(gc.cleanupRetries, retryKey)
				gc.mu.Unlock()
				cleaned++
			}
		}
	}

	// Now check each connected worker for orphaned sessions.
	for _, conn := range gc.registry.List() {
		if ctx.Err() != nil {
			break
		}
		workerID := conn.Worker.ID
		sessions, err := gc.store.Sessions().ListByWorker(ctx, workerID)
		if err != nil {
			gc.logger.Warn("gc: failed to list sessions for worker", "worker", workerID, "err", err)
			continue
		}

		for _, s := range sessions {
			// If a session is terminated, ask the worker to clean it up.
			if s.Status == "terminated" {
				if err := gc.sendCleanup(workerID, s.ID); err != nil {
					gc.mu.Lock()
					gc.pendingCleanup[workerID] = append(gc.pendingCleanup[workerID], s.ID)
					gc.mu.Unlock()
				} else {
					cleaned++
				}
			}
		}
	}

	return cleaned
}

func (gc *GarbageCollector) sendCleanup(workerID, sessionID string) error {
	cmd := worker.WorkerCommand{
		ID:   uuid.New().String(),
		Type: "stop_session",
		Data: worker.StopSessionData{SessionID: sessionID},
	}
	return gc.registry.SendCommand(workerID, cmd)
}

// cleanupImages removes images that have not been used past their TTL.
func (gc *GarbageCollector) cleanupImages(imageTTL time.Duration) int {
	cutoff := time.Now().Add(-imageTTL)
	cleaned := 0
	for _, img := range gc.images.List() {
		if img.CreatedAt.Before(cutoff) {
			if err := gc.images.Delete(img.ID); err != nil {
				gc.logger.Warn("gc: failed to delete image", "id", img.ID, "err", err)
				continue
			}
			cleaned++
		}
	}
	return cleaned
}

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
