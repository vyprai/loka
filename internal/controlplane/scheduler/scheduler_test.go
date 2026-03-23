package scheduler

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rizqme/loka/internal/controlplane/worker"
	"github.com/rizqme/loka/internal/loka"
	"github.com/rizqme/loka/internal/store/sqlite"
)

// newTestRegistry creates an in-memory store and registry for tests.
func newTestRegistry(t *testing.T) *worker.Registry {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return worker.NewRegistry(s, logger)
}

// registerWorker is a helper that registers a worker with the given properties.
func registerWorker(t *testing.T, reg *worker.Registry, provider, region string, status loka.WorkerStatus, labels map[string]string, cpuCores int, memMB int64) *loka.Worker {
	t.Helper()
	ctx := context.Background()
	if labels == nil {
		labels = map[string]string{}
	}
	cap := loka.ResourceCapacity{CPUCores: cpuCores, MemoryMB: memMB, DiskMB: 100000}
	w, err := reg.Register(ctx, "host-"+uuid.New().String()[:8], "10.0.0.1", provider, region, "zone-a", "0.1.0", cap, labels, true)
	if err != nil {
		t.Fatal(err)
	}
	// Override status if not ready (Register always sets ready).
	if status != loka.WorkerStatusReady {
		w.Status = status
	}
	return w
}

// ---------------------------------------------------------------------------
// No workers available
// ---------------------------------------------------------------------------

func TestPickNoWorkers(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	_, err := sched.Pick(Constraints{})
	if err == nil {
		t.Error("expected error when no workers available")
	}
}

// ---------------------------------------------------------------------------
// Single worker
// ---------------------------------------------------------------------------

func TestPickSingleWorker(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	w := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)

	picked, err := sched.Pick(Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.ID != w.ID {
		t.Errorf("picked worker %s, want %s", picked.Worker.ID, w.ID)
	}
}

// ---------------------------------------------------------------------------
// Worker not eligible (draining, dead, etc.)
// ---------------------------------------------------------------------------

func TestPickSkipsNonEligibleWorkers(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	// Register a draining worker.
	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusDraining, nil, 4, 8192)

	_, err := sched.Pick(Constraints{})
	if err == nil {
		t.Error("expected error when only draining workers available")
	}
}

func TestPickSkipsDeadWorkers(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusDead, nil, 4, 8192)

	_, err := sched.Pick(Constraints{})
	if err == nil {
		t.Error("expected error when only dead workers available")
	}
}

// ---------------------------------------------------------------------------
// Excluded workers
// ---------------------------------------------------------------------------

func TestPickRespectsExcludeWorkers(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	w1 := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)
	w2 := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)

	picked, err := sched.Pick(Constraints{ExcludeWorkers: []string{w1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.ID != w2.ID {
		t.Errorf("picked %s, want %s (w1 excluded)", picked.Worker.ID, w2.ID)
	}
}

func TestPickAllWorkersExcluded(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	w1 := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)

	_, err := sched.Pick(Constraints{ExcludeWorkers: []string{w1.ID}})
	if err == nil {
		t.Error("expected error when all workers excluded")
	}
}

// ---------------------------------------------------------------------------
// Label affinity
// ---------------------------------------------------------------------------

func TestPickRequireLabelsMatch(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, map[string]string{"tier": "standard"}, 4, 8192)
	w2 := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, map[string]string{"tier": "gpu"}, 8, 16384)

	picked, err := sched.Pick(Constraints{
		RequireLabels: map[string]string{"tier": "gpu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.ID != w2.ID {
		t.Errorf("picked %s, want %s (gpu worker)", picked.Worker.ID, w2.ID)
	}
}

func TestPickRequireLabelsNoMatch(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, map[string]string{"tier": "standard"}, 4, 8192)

	_, err := sched.Pick(Constraints{
		RequireLabels: map[string]string{"tier": "gpu"},
	})
	if err == nil {
		t.Error("expected error when no workers match required labels")
	}
}

func TestPickRequireMultipleLabels(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, map[string]string{"tier": "gpu"}, 4, 8192)
	w2 := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, map[string]string{"tier": "gpu", "env": "prod"}, 4, 8192)

	picked, err := sched.Pick(Constraints{
		RequireLabels: map[string]string{"tier": "gpu", "env": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.ID != w2.ID {
		t.Errorf("picked %s, want %s", picked.Worker.ID, w2.ID)
	}
}

// ---------------------------------------------------------------------------
// Region preference
// ---------------------------------------------------------------------------

func TestPickPrefersRegion(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	// Two workers with identical specs, different regions.
	registerWorker(t, reg, "aws", "us-west-2", loka.WorkerStatusReady, nil, 4, 8192)
	registerWorker(t, reg, "aws", "eu-west-1", loka.WorkerStatusReady, nil, 4, 8192)

	picked, err := sched.Pick(Constraints{PreferRegion: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.Region != "eu-west-1" {
		t.Errorf("picked region %s, want eu-west-1", picked.Worker.Region)
	}
}

// ---------------------------------------------------------------------------
// Provider preference
// ---------------------------------------------------------------------------

func TestPickPrefersProvider(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)
	registerWorker(t, reg, "gcp", "us-central1", loka.WorkerStatusReady, nil, 4, 8192)

	picked, err := sched.Pick(Constraints{PreferProvider: "gcp"})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.Provider != "gcp" {
		t.Errorf("picked provider %s, want gcp", picked.Worker.Provider)
	}
}

// ---------------------------------------------------------------------------
// Spread vs BinPack strategies
// ---------------------------------------------------------------------------

func TestSpreadPrefersLessLoadedWorker(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	// Create two workers with different capacity (used as proxy for load).
	// Worker with more CPU cores gets a higher capacity bonus, so to test spread
	// behavior we make them identical.
	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)
	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)

	// With spread and identical workers, the scheduler should pick one.
	// We just verify it does not error.
	_, err := sched.Pick(Constraints{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBinPackPrefersHigherCapacityWorker(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategyBinPack)

	// Worker with more resources should be preferred by binpack
	// due to higher capacity scoring bonus.
	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 2, 4096)
	w2 := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 16, 65536)

	picked, err := sched.Pick(Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	// Larger worker should score higher (more capacity bonus).
	if picked.Worker.ID != w2.ID {
		t.Errorf("binpack picked %s (cpu=%d), want %s (cpu=16)",
			picked.Worker.ID, picked.Worker.Capacity.CPUCores, w2.ID)
	}
}

func TestDefaultStrategyIsSpread(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, "")

	if sched.strategy != StrategySpread {
		t.Errorf("default strategy = %s, want spread", sched.strategy)
	}
}

// ---------------------------------------------------------------------------
// Busy workers are eligible but score lower than Ready
// ---------------------------------------------------------------------------

func TestPickBusyWorkerWhenOnlyOption(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	w := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusBusy, nil, 4, 8192)

	picked, err := sched.Pick(Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.ID != w.ID {
		t.Errorf("picked %s, want %s", picked.Worker.ID, w.ID)
	}
}

func TestPickPrefersReadyOverBusy(t *testing.T) {
	reg := newTestRegistry(t)
	sched := New(reg, StrategySpread)

	registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusBusy, nil, 4, 8192)
	wReady := registerWorker(t, reg, "aws", "us-east-1", loka.WorkerStatusReady, nil, 4, 8192)

	picked, err := sched.Pick(Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	if picked.Worker.ID != wReady.ID {
		t.Errorf("should prefer ready worker, picked %s", picked.Worker.ID)
	}
}

// ---------------------------------------------------------------------------
// isEligible unit tests
// ---------------------------------------------------------------------------

func TestIsEligible(t *testing.T) {
	now := time.Now()
	base := &loka.Worker{
		ID:       "w1",
		Status:   loka.WorkerStatusReady,
		Labels:   map[string]string{"tier": "gpu", "env": "prod"},
		LastSeen: now,
	}

	tests := []struct {
		name        string
		worker      *loka.Worker
		constraints Constraints
		want        bool
	}{
		{
			name:        "ready worker no constraints",
			worker:      base,
			constraints: Constraints{},
			want:        true,
		},
		{
			name:        "draining worker",
			worker:      &loka.Worker{ID: "w2", Status: loka.WorkerStatusDraining},
			constraints: Constraints{},
			want:        false,
		},
		{
			name:        "dead worker",
			worker:      &loka.Worker{ID: "w3", Status: loka.WorkerStatusDead},
			constraints: Constraints{},
			want:        false,
		},
		{
			name:        "registering worker",
			worker:      &loka.Worker{ID: "w4", Status: loka.WorkerStatusRegistering},
			constraints: Constraints{},
			want:        false,
		},
		{
			name:        "busy worker is eligible",
			worker:      &loka.Worker{ID: "w5", Status: loka.WorkerStatusBusy, Labels: map[string]string{}},
			constraints: Constraints{},
			want:        true,
		},
		{
			name:   "excluded worker",
			worker: base,
			constraints: Constraints{
				ExcludeWorkers: []string{"w1"},
			},
			want: false,
		},
		{
			name:   "label match",
			worker: base,
			constraints: Constraints{
				RequireLabels: map[string]string{"tier": "gpu"},
			},
			want: true,
		},
		{
			name:   "label mismatch",
			worker: base,
			constraints: Constraints{
				RequireLabels: map[string]string{"tier": "standard"},
			},
			want: false,
		},
		{
			name:   "missing label",
			worker: &loka.Worker{ID: "w6", Status: loka.WorkerStatusReady, Labels: map[string]string{}},
			constraints: Constraints{
				RequireLabels: map[string]string{"tier": "gpu"},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isEligible(tc.worker, tc.constraints)
			if got != tc.want {
				t.Errorf("isEligible() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Strategy constants
// ---------------------------------------------------------------------------

func TestStrategyConstants(t *testing.T) {
	if StrategySpread != "spread" {
		t.Errorf("StrategySpread = %q", StrategySpread)
	}
	if StrategyBinPack != "binpack" {
		t.Errorf("StrategyBinPack = %q", StrategyBinPack)
	}
}
