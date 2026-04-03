package worker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store/sqlite"
)

func setupTestRegistry(t *testing.T) *Registry {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(context.Background())
	t.Cleanup(func() { s.Close() })
	return NewRegistry(s, slog.Default(), nil)
}

func TestCleanupDead_RemovesDeadWorkers(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	w1, _ := reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)
	w2, _ := reg.Register(ctx, "host2", "10.0.0.2", "local", "", "", "", loka.ResourceCapacity{}, nil, false)
	w3, _ := reg.Register(ctx, "host3", "10.0.0.3", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	// Mark w1 and w3 as dead.
	if conn, ok := reg.Get(w1.ID); ok {
		conn.Worker.Status = loka.WorkerStatusDead
	}
	if conn, ok := reg.Get(w3.ID); ok {
		conn.Worker.Status = loka.WorkerStatusDead
	}

	count := reg.CleanupDead()
	if count != 2 {
		t.Errorf("expected 2 cleaned up, got %d", count)
	}

	// w2 should still be in registry.
	if _, ok := reg.Get(w2.ID); !ok {
		t.Error("w2 should still be in registry")
	}

	// w1 and w3 should be gone.
	if _, ok := reg.Get(w1.ID); ok {
		t.Error("w1 should be removed")
	}
	if _, ok := reg.Get(w3.ID); ok {
		t.Error("w3 should be removed")
	}
}

func TestCleanupDead_NoDeadWorkers(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	count := reg.CleanupDead()
	if count != 0 {
		t.Errorf("expected 0 cleaned up, got %d", count)
	}
}

func TestCleanupDead_EmptyRegistry(t *testing.T) {
	reg := setupTestRegistry(t)
	count := reg.CleanupDead()
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestListHealthy_ExcludesDead(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	w1, _ := reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)
	reg.Register(ctx, "host2", "10.0.0.2", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	// Mark w1 as dead.
	if conn, ok := reg.Get(w1.ID); ok {
		conn.Worker.Status = loka.WorkerStatusDead
	}

	healthy, err := reg.ListHealthy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(healthy) != 1 {
		t.Errorf("expected 1 healthy, got %d", len(healthy))
	}
	for _, id := range healthy {
		if id == w1.ID {
			t.Error("dead worker should not be in healthy list")
		}
	}
}

func TestListHealthy_IncludesBusy(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	w1, _ := reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	if conn, ok := reg.Get(w1.ID); ok {
		conn.Worker.Status = loka.WorkerStatusBusy
	}

	healthy, _ := reg.ListHealthy(ctx)
	if len(healthy) != 1 {
		t.Errorf("expected busy worker in healthy list, got %d", len(healthy))
	}
}

func TestOnWorkerJoin_CalledOnRegister(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	done := make(chan struct{})
	reg.SetOnWorkerJoin(func(_ context.Context) {
		close(done)
	})

	reg.Register(ctx, "host1", "10.0.0.1", "local", "", "", "", loka.ResourceCapacity{}, nil, false)

	select {
	case <-done:
		// Hook was called.
	case <-time.After(time.Second):
		t.Error("onWorkerJoin hook was not called within 1s")
	}
}
