package worker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store/sqlite"
)

func TestGracePeriod_SkipsChecksOnStart(t *testing.T) {
	s, _ := sqlite.New(":memory:")
	s.Migrate(context.Background())
	defer s.Close()

	reg := NewRegistry(s, slog.Default(), nil)
	ctx := context.Background()

	// Register a worker.
	w, _ := reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	// Set last seen to 1 minute ago (would be dead normally).
	w.LastSeen = time.Now().Add(-1 * time.Minute)
	s.Workers().Update(ctx, w)

	monitor := NewMonitor(reg, s, nil, MonitorConfig{
		SuspectAfter:  5 * time.Second,
		DeadAfter:     10 * time.Second,
		CheckInterval: 100 * time.Millisecond,
	}, slog.Default(), nil)

	monCtx, monCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer monCancel()

	go monitor.Start(monCtx)
	<-monCtx.Done()

	// Worker should NOT be marked dead during grace period (30s).
	conn, ok := reg.Get(w.ID)
	if !ok {
		t.Fatal("worker should still be in registry")
	}
	if conn.Worker.Status == loka.WorkerStatusDead {
		t.Error("worker should NOT be marked dead during grace period")
	}
}

func TestVolumeFailoverFunc_CalledOnDeath(t *testing.T) {
	s, _ := sqlite.New(":memory:")
	s.Migrate(context.Background())
	defer s.Close()

	reg := NewRegistry(s, slog.Default(), nil)
	ctx := context.Background()

	w, _ := reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	var failoverCalled bool
	var failoverWorkerID string
	var mu sync.Mutex

	monitor := NewMonitor(reg, s, nil, DefaultMonitorConfig(), slog.Default(), nil)
	monitor.SetVolumeFailoverFunc(func(_ context.Context, deadWorkerID string) error {
		mu.Lock()
		failoverCalled = true
		failoverWorkerID = deadWorkerID
		mu.Unlock()
		return nil
	})

	// Simulate worker death by directly calling handleWorkerDead.
	monitor.handleWorkerDead(ctx, w)

	mu.Lock()
	defer mu.Unlock()
	if !failoverCalled {
		t.Error("volume failover function should be called on worker death")
	}
	if failoverWorkerID != w.ID {
		t.Errorf("expected worker ID %s, got %s", w.ID, failoverWorkerID)
	}
}

func TestHandleWorkerDead_NoSessions_NoError(t *testing.T) {
	s, _ := sqlite.New(":memory:")
	s.Migrate(context.Background())
	defer s.Close()

	reg := NewRegistry(s, slog.Default(), nil)
	ctx := context.Background()

	w1, _ := reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	monitor := NewMonitor(reg, s, nil, DefaultMonitorConfig(), slog.Default(), nil)
	// Should not panic or error when worker has no sessions.
	monitor.handleWorkerDead(ctx, w1)

	conn, ok := reg.Get(w1.ID)
	if !ok {
		t.Fatal("worker should still be in registry")
	}
	if conn.Worker.Status != loka.WorkerStatusDead {
		t.Errorf("expected dead status, got %s", conn.Worker.Status)
	}
}
