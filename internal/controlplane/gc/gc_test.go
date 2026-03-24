package gc

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/config"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
	"github.com/vyprai/loka/internal/store/sqlite"
)

func setupGCTest(t *testing.T, ret config.RetentionConfig) (*GarbageCollector, *sqlite.Store) {
	t.Helper()

	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	tmpDir := t.TempDir()
	objStore, err := local.New(tmpDir)
	if err != nil {
		t.Fatalf("create local objstore: %v", err)
	}
	imgMgr := image.NewManager(objStore, tmpDir, logger)
	reg := worker.NewRegistry(db, logger)

	gc := New(db, objStore, reg, imgMgr, ret, logger)
	return gc, db
}

func TestNew(t *testing.T) {
	ret := config.RetentionConfig{
		SessionTTL:      "1h",
		ExecutionTTL:    "1h",
		TokenTTL:        "1h",
		ImageTTL:        "1h",
		CleanupInterval: "10m",
	}
	gc, _ := setupGCTest(t, ret)

	if gc == nil {
		t.Fatal("expected non-nil GarbageCollector")
	}
	if gc.store == nil {
		t.Error("expected non-nil store")
	}
	if gc.pendingCleanup == nil {
		t.Error("expected non-nil pendingCleanup map")
	}
}

func TestSweep(t *testing.T) {
	// Use a very short TTL so old data is immediately eligible.
	ret := config.RetentionConfig{
		SessionTTL:      "1ms",
		ExecutionTTL:    "1ms",
		TokenTTL:        "1ms",
		ImageTTL:        "720h",
		CleanupInterval: "1h",
	}
	gc, db := setupGCTest(t, ret)
	ctx := context.Background()

	// Insert test sessions: 1 old terminated, 1 recent terminated, 1 running.
	oldTime := time.Now().Add(-48 * time.Hour)
	recentTime := time.Now()

	oldTerminated := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "old-terminated",
		Status:    loka.SessionStatusTerminated,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{},
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	recentTerminated := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "recent-terminated",
		Status:    loka.SessionStatusTerminated,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{},
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: recentTime,
		UpdatedAt: recentTime,
	}
	runningSession := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "running",
		Status:    loka.SessionStatusRunning,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{},
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	for _, s := range []*loka.Session{oldTerminated, recentTerminated, runningSession} {
		if err := db.Sessions().Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	// Insert test executions: 2 old completed, 1 running.
	// Executions need a parent session. Use the running session to avoid FK issues.
	oldExec1 := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: runningSession.ID,
		Status:    loka.ExecStatusSuccess,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	oldExec2 := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: runningSession.ID,
		Status:    loka.ExecStatusFailed,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	runningExec := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: runningSession.ID,
		Status:    loka.ExecStatusRunning,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	for _, e := range []*loka.Execution{oldExec1, oldExec2, runningExec} {
		if err := db.Executions().Create(ctx, e); err != nil {
			t.Fatalf("create execution: %v", err)
		}
	}

	// Insert test tokens: 1 expired old, 1 valid.
	expiredToken := &loka.WorkerToken{
		ID:        uuid.New().String(),
		Name:      "expired-token",
		Token:     loka.GenerateToken(),
		ExpiresAt: oldTime,
		CreatedAt: oldTime,
	}
	validToken := &loka.WorkerToken{
		ID:        uuid.New().String(),
		Name:      "valid-token",
		Token:     loka.GenerateToken(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: recentTime,
	}
	for _, tok := range []*loka.WorkerToken{expiredToken, validToken} {
		if err := db.Tokens().Create(ctx, tok); err != nil {
			t.Fatalf("create token: %v", err)
		}
	}

	// Give time for TTL to expire.
	time.Sleep(5 * time.Millisecond)

	result := gc.Sweep(ctx)

	if result == nil {
		t.Fatal("expected non-nil SweepResult")
	}
	// The old terminated session should be purged; recent terminated kept (TTL is 1ms but
	// the recent one was just created, so it depends on timing). At minimum, old one purged.
	if result.SessionsPurged < 1 {
		t.Errorf("SessionsPurged = %d, want >= 1", result.SessionsPurged)
	}
	// 2 old completed executions should be purged.
	if result.ExecutionsPurged < 2 {
		t.Errorf("ExecutionsPurged = %d, want >= 2", result.ExecutionsPurged)
	}
	// 1 expired token should be purged.
	if result.TokensPurged < 1 {
		t.Errorf("TokensPurged = %d, want >= 1", result.TokensPurged)
	}

	// Verify running session still exists.
	_, err := db.Sessions().Get(ctx, runningSession.ID)
	if err != nil {
		t.Errorf("running session should still exist: %v", err)
	}

	// Verify running execution still exists.
	_, err = db.Executions().Get(ctx, runningExec.ID)
	if err != nil {
		t.Errorf("running execution should still exist: %v", err)
	}

	// Verify valid token still exists.
	_, err = db.Tokens().Get(ctx, validToken.ID)
	if err != nil {
		t.Errorf("valid token should still exist: %v", err)
	}

	// Verify old terminated session was purged.
	_, err = db.Sessions().Get(ctx, oldTerminated.ID)
	if err == nil {
		t.Error("old terminated session should have been purged")
	}
}

func TestSweepDryRun(t *testing.T) {
	ret := config.RetentionConfig{
		SessionTTL:      "1ms",
		ExecutionTTL:    "1ms",
		TokenTTL:        "1ms",
		ImageTTL:        "720h",
		CleanupInterval: "1h",
	}
	gc, db := setupGCTest(t, ret)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour)

	// Create an old terminated session.
	oldSess := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "old-terminated",
		Status:    loka.SessionStatusTerminated,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{},
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	if err := db.Sessions().Create(ctx, oldSess); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	result := gc.SweepDryRun(ctx)

	if result == nil {
		t.Fatal("expected non-nil SweepResult")
	}
	// Dry run should count the old terminated session as eligible.
	if result.SessionsPurged < 1 {
		t.Errorf("SweepDryRun SessionsPurged = %d, want >= 1", result.SessionsPurged)
	}

	// Verify nothing was actually deleted.
	_, err := db.Sessions().Get(ctx, oldSess.ID)
	if err != nil {
		t.Error("dry run should not have deleted the session")
	}
}

func TestSweep_RespectsContext(t *testing.T) {
	ret := config.RetentionConfig{
		SessionTTL:      "1ms",
		ExecutionTTL:    "1ms",
		TokenTTL:        "1ms",
		ImageTTL:        "720h",
		CleanupInterval: "1h",
	}
	gc, db := setupGCTest(t, ret)

	oldTime := time.Now().Add(-48 * time.Hour)

	// Create data that should be eligible for cleanup.
	sess := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "should-survive",
		Status:    loka.SessionStatusTerminated,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{},
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	if err := db.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	// Cancel context before calling Sweep.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := gc.Sweep(ctx)
	if result == nil {
		t.Fatal("expected non-nil SweepResult even with cancelled context")
	}

	// With a cancelled context, the sweep should bail early.
	// The session purge is the first step, so it should be skipped.
	if result.SessionsPurged != 0 {
		t.Errorf("SessionsPurged = %d, want 0 (context was cancelled)", result.SessionsPurged)
	}
}

func TestRun_StartsAndCleansUp(t *testing.T) {
	ret := config.RetentionConfig{
		SessionTTL:      "1ms",
		ExecutionTTL:    "1ms",
		TokenTTL:        "1ms",
		ImageTTL:        "720h",
		CleanupInterval: "100ms", // Very short interval for testing.
	}
	gc, db := setupGCTest(t, ret)
	ctx := context.Background()

	// Create an old terminated session.
	oldTime := time.Now().Add(-48 * time.Hour)
	oldSess := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "gc-run-test",
		Status:    loka.SessionStatusTerminated,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{},
		VCPUs:     1,
		MemoryMB:  512,
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	if err := db.Sessions().Create(ctx, oldSess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	// Verify the session exists before GC runs.
	_, err := db.Sessions().Get(ctx, oldSess.ID)
	if err != nil {
		t.Fatalf("session should exist before GC: %v", err)
	}

	// Start the GC in a goroutine with a cancellable context.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		gc.Run(runCtx)
		close(done)
	}()

	// Wait for GC to run at least one sweep. The Run() method calls Sweep()
	// immediately on start, then enters a ticker loop. We poll for the result
	// rather than relying on a fixed sleep.
	deadline := time.After(5 * time.Second)
	for {
		result := gc.LastResult()
		if result != nil && result.SessionsPurged >= 1 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			result := gc.LastResult()
			if result == nil {
				t.Fatal("GC never ran (LastResult is nil)")
			}
			t.Fatalf("timed out waiting for GC to purge session (SessionsPurged=%d)", result.SessionsPurged)
		case <-time.After(50 * time.Millisecond):
			// Keep polling.
		}
	}
	cancel()
	<-done

	// Verify the old terminated session was cleaned up by Run().
	_, err = db.Sessions().Get(ctx, oldSess.ID)
	if err == nil {
		t.Error("expected old terminated session to be purged by GC Run()")
	}

	// Verify LastResult was populated.
	result := gc.LastResult()
	if result == nil {
		t.Fatal("expected non-nil LastResult after Run()")
	}
	if result.SessionsPurged < 1 {
		t.Errorf("SessionsPurged = %d, want >= 1", result.SessionsPurged)
	}
}

func TestLastResult(t *testing.T) {
	ret := config.RetentionConfig{
		SessionTTL:      "168h",
		ExecutionTTL:    "72h",
		TokenTTL:        "24h",
		ImageTTL:        "720h",
		CleanupInterval: "1h",
	}
	gc, _ := setupGCTest(t, ret)

	// Before any sweep, LastResult should be nil.
	if gc.LastResult() != nil {
		t.Error("expected nil LastResult before first sweep")
	}

	// Run a sweep.
	ctx := context.Background()
	gc.Sweep(ctx)

	// After sweep, LastResult should be populated.
	result := gc.LastResult()
	if result == nil {
		t.Fatal("expected non-nil LastResult after sweep")
	}
	if result.Timestamp.IsZero() {
		t.Error("expected non-zero Timestamp in LastResult")
	}
	if result.Duration == 0 {
		t.Error("expected non-zero Duration in LastResult")
	}
}
