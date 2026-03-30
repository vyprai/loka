package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// --- mock session repo ---

type mockSessionRepo struct {
	mu       sync.Mutex
	sessions map[string]*loka.Session
}

func newMockSessionRepo() *mockSessionRepo {
	return &mockSessionRepo{sessions: make(map[string]*loka.Session)}
}

func (m *mockSessionRepo) Create(_ context.Context, s *loka.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *mockSessionRepo) Get(_ context.Context, id string) (*loka.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return s, nil
}

func (m *mockSessionRepo) Update(_ context.Context, s *loka.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *mockSessionRepo) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *mockSessionRepo) List(_ context.Context, _ store.SessionFilter) ([]*loka.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*loka.Session
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out, nil
}

func (m *mockSessionRepo) ListByWorker(_ context.Context, workerID string) ([]*loka.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*loka.Session
	for _, s := range m.sessions {
		if s.WorkerID == workerID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *mockSessionRepo) DeleteTerminatedBefore(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

func (m *mockSessionRepo) getSession(id string) *loka.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// --- mock store with sessions ---

type mockStoreWithSessions struct {
	workerRepo  *mockWorkerRepo
	sessionRepo *mockSessionRepo
}

func newMockStoreWithSessions() *mockStoreWithSessions {
	return &mockStoreWithSessions{
		workerRepo:  newMockWorkerRepo(),
		sessionRepo: newMockSessionRepo(),
	}
}

func (s *mockStoreWithSessions) Sessions() store.SessionRepository       { return s.sessionRepo }
func (s *mockStoreWithSessions) Executions() store.ExecutionRepository   { return nil }
func (s *mockStoreWithSessions) Checkpoints() store.CheckpointRepository { return nil }
func (s *mockStoreWithSessions) Workers() store.WorkerRepository         { return s.workerRepo }
func (s *mockStoreWithSessions) Tokens() store.TokenRepository           { return nil }
func (s *mockStoreWithSessions) Services() store.ServiceRepository       { return nil }
func (s *mockStoreWithSessions) Volumes() store.VolumeRepository         { return nil }
func (s *mockStoreWithSessions) Tasks() store.TaskRepository             { return nil }
func (s *mockStoreWithSessions) Migrate(_ context.Context) error         { return nil }
func (s *mockStoreWithSessions) Close() error                            { return nil }

// --- tests ---

func TestMigrationRetry_AbandonedAfter3Failures(t *testing.T) {
	logger := slog.Default()
	st := newMockStoreWithSessions()
	registry := NewRegistry(st, logger)

	ctx := context.Background()

	// Register a dead worker and a healthy target.
	deadWorker, err := registry.Register(ctx, "dead-host", "10.0.0.1", "aws", "us-east-1", "zone-a", "v1",
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192}, nil, true)
	if err != nil {
		t.Fatalf("register dead worker: %v", err)
	}
	targetWorker, err := registry.Register(ctx, "healthy-host", "10.0.0.2", "aws", "us-east-1", "zone-b", "v1",
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192}, nil, true)
	if err != nil {
		t.Fatalf("register target worker: %v", err)
	}
	_ = targetWorker

	// Create a running session on the dead worker.
	sess := &loka.Session{
		ID:       "sess-1",
		Name:     "test-session",
		Status:   loka.SessionStatusRunning,
		WorkerID: deadWorker.ID,
	}
	if err := st.sessionRepo.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// migrateFunc always fails.
	alwaysFail := func(_ context.Context, _, _ string) error {
		return fmt.Errorf("migration failed")
	}

	// Make the dead worker actually old enough to be detected as dead.
	deadWorker.LastSeen = time.Now().Add(-1 * time.Minute)
	deadWorker.Status = loka.WorkerStatusReady

	mon := NewMonitor(registry, st, alwaysFail, MonitorConfig{
		SuspectAfter:  5 * time.Millisecond,
		DeadAfter:     10 * time.Millisecond,
		CheckInterval: 5 * time.Millisecond,
	}, logger)

	// Run check() multiple times to trigger retries.
	// After each check, the worker is marked dead and the session is set to "creating".
	// We need to reset the worker status and session state so the next check re-processes it.
	for i := 0; i < 4; i++ {
		mon.check(ctx)

		// Reset worker so the next check() sees it as needing dead-handling again.
		deadWorker.Status = loka.WorkerStatusReady
		deadWorker.LastSeen = time.Now().Add(-1 * time.Minute)

		// Reset session to "running" on the dead worker so handleWorkerDead picks it up.
		s := st.sessionRepo.getSession("sess-1")
		if s != nil && s.Status != loka.SessionStatusError {
			s.Status = loka.SessionStatusRunning
			s.WorkerID = deadWorker.ID
			st.sessionRepo.Update(ctx, s)
		}
	}

	// After 3+ failures, the session should be marked as error.
	got := st.sessionRepo.getSession("sess-1")
	if got == nil {
		t.Fatal("session not found after migration retries")
	}
	if got.Status != loka.SessionStatusError {
		t.Errorf("session status = %q, want %q after 3 failed migrations", got.Status, loka.SessionStatusError)
	}

	// migrationTries should be cleaned up.
	if count, ok := mon.migrationTries["sess-1"]; ok {
		t.Errorf("migrationTries[sess-1] = %d, want deleted", count)
	}
}

func TestMigrationRetry_SuccessResetsCount(t *testing.T) {
	logger := slog.Default()
	st := newMockStoreWithSessions()
	registry := NewRegistry(st, logger)

	ctx := context.Background()

	// Register two workers.
	deadWorker, err := registry.Register(ctx, "dead-host", "10.0.0.1", "aws", "us-east-1", "zone-a", "v1",
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192}, nil, true)
	if err != nil {
		t.Fatalf("register dead worker: %v", err)
	}
	_, err = registry.Register(ctx, "healthy-host", "10.0.0.2", "aws", "us-east-1", "zone-b", "v1",
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192}, nil, true)
	if err != nil {
		t.Fatalf("register target worker: %v", err)
	}

	// Create a running session on the dead worker.
	sess := &loka.Session{
		ID:       "sess-2",
		Name:     "test-session-2",
		Status:   loka.SessionStatusRunning,
		WorkerID: deadWorker.ID,
	}
	if err := st.sessionRepo.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// migrateFunc fails once then succeeds.
	var mu sync.Mutex
	callCount := 0
	failThenSucceed := func(_ context.Context, _, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		if callCount <= 1 {
			return fmt.Errorf("transient failure")
		}
		return nil
	}

	deadWorker.LastSeen = time.Now().Add(-1 * time.Minute)
	deadWorker.Status = loka.WorkerStatusReady

	mon := NewMonitor(registry, st, failThenSucceed, MonitorConfig{
		SuspectAfter:  5 * time.Millisecond,
		DeadAfter:     10 * time.Millisecond,
		CheckInterval: 5 * time.Millisecond,
	}, logger)

	// First check: worker goes dead, migration fails (try 1).
	mon.check(ctx)
	if count := mon.migrationTries["sess-2"]; count != 1 {
		t.Errorf("after first check: migrationTries = %d, want 1", count)
	}

	// Reset session status so handleWorkerDead processes it again.
	// (The monitor sets it to "creating" before migration.)
	got := st.sessionRepo.getSession("sess-2")
	if got == nil {
		t.Fatal("session not found")
	}
	got.WorkerID = deadWorker.ID
	got.Status = loka.SessionStatusRunning
	st.sessionRepo.Update(ctx, got)

	// Mark worker as ready again so check() re-detects it as dead.
	deadWorker.Status = loka.WorkerStatusReady

	// Second check: migration succeeds.
	mon.check(ctx)

	// migrationTries should be cleared on success.
	if count, ok := mon.migrationTries["sess-2"]; ok {
		t.Errorf("migrationTries[sess-2] = %d after success, want deleted", count)
	}
}
