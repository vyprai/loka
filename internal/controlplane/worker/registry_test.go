package worker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// --- minimal mock store ---

type mockWorkerRepo struct {
	mu      sync.Mutex
	workers map[string]*loka.Worker
}

func newMockWorkerRepo() *mockWorkerRepo {
	return &mockWorkerRepo{workers: make(map[string]*loka.Worker)}
}

func (m *mockWorkerRepo) Create(_ context.Context, w *loka.Worker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers[w.ID] = w
	return nil
}
func (m *mockWorkerRepo) Get(_ context.Context, id string) (*loka.Worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[id]
	if !ok {
		return nil, nil
	}
	return w, nil
}
func (m *mockWorkerRepo) Update(_ context.Context, w *loka.Worker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers[w.ID] = w
	return nil
}
func (m *mockWorkerRepo) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.workers, id)
	return nil
}
func (m *mockWorkerRepo) List(_ context.Context, _ store.WorkerFilter) ([]*loka.Worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*loka.Worker
	for _, w := range m.workers {
		out = append(out, w)
	}
	return out, nil
}
func (m *mockWorkerRepo) UpdateHeartbeat(_ context.Context, id string, hb *loka.Heartbeat) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[id]; ok {
		w.LastSeen = hb.Timestamp
		w.Status = hb.Status
	}
	return nil
}

type mockStore struct {
	workerRepo *mockWorkerRepo
}

func newMockStore() *mockStore {
	return &mockStore{workerRepo: newMockWorkerRepo()}
}

func (s *mockStore) Sessions() store.SessionRepository       { return nil }
func (s *mockStore) Executions() store.ExecutionRepository   { return nil }
func (s *mockStore) Checkpoints() store.CheckpointRepository { return nil }
func (s *mockStore) Workers() store.WorkerRepository         { return s.workerRepo }
func (s *mockStore) Tokens() store.TokenRepository           { return nil }
func (s *mockStore) Services() store.ServiceRepository       { return nil }
func (s *mockStore) Volumes() store.VolumeRepository         { return nil }
func (s *mockStore) Tasks() store.TaskRepository             { return nil }
func (s *mockStore) Migrate(_ context.Context) error         { return nil }
func (s *mockStore) Close() error                            { return nil }

// --- tests ---

func newTestRegistry() *Registry {
	logger := slog.Default()
	return NewRegistry(newMockStore(), logger)
}

func registerTestWorker(t *testing.T, r *Registry) *loka.Worker {
	t.Helper()
	ctx := context.Background()
	w, err := r.Register(ctx, "host1", "10.0.0.1", "aws", "us-east-1", "us-east-1a", "v1.0.0",
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192, DiskMB: 100000},
		map[string]string{"env": "test"}, true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return w
}

func TestRegister(t *testing.T) {
	r := newTestRegistry()
	w := registerTestWorker(t, r)

	if w.ID == "" {
		t.Error("worker ID is empty")
	}
	if w.Hostname != "host1" {
		t.Errorf("Hostname = %q, want %q", w.Hostname, "host1")
	}
	if w.Status != loka.WorkerStatusReady {
		t.Errorf("Status = %q, want %q", w.Status, loka.WorkerStatusReady)
	}
}

func TestGet_Found(t *testing.T) {
	r := newTestRegistry()
	w := registerTestWorker(t, r)

	conn, ok := r.Get(w.ID)
	if !ok {
		t.Fatal("Get: worker not found")
	}
	if conn.Worker.ID != w.ID {
		t.Errorf("ID = %q, want %q", conn.Worker.ID, w.ID)
	}
}

func TestGet_NotFound(t *testing.T) {
	r := newTestRegistry()

	_, ok := r.Get("nonexistent")
	if ok {
		t.Error("Get: expected not found")
	}
}

func TestList_Empty(t *testing.T) {
	r := newTestRegistry()
	conns := r.List()
	if len(conns) != 0 {
		t.Errorf("List: got %d workers, want 0", len(conns))
	}
}

func TestList_MultipleWorkers(t *testing.T) {
	r := newTestRegistry()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := r.Register(ctx, "host", "10.0.0.1", "aws", "us-east-1", "zone", "v1",
			loka.ResourceCapacity{}, nil, true)
		if err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}

	conns := r.List()
	if len(conns) != 3 {
		t.Errorf("List: got %d workers, want 3", len(conns))
	}
}

func TestUnregister(t *testing.T) {
	r := newTestRegistry()
	w := registerTestWorker(t, r)

	r.Unregister(w.ID)

	_, ok := r.Get(w.ID)
	if ok {
		t.Error("worker still found after Unregister")
	}

	conns := r.List()
	if len(conns) != 0 {
		t.Errorf("List after remove: got %d, want 0", len(conns))
	}
}

func TestUpdateHeartbeat(t *testing.T) {
	r := newTestRegistry()
	w := registerTestWorker(t, r)

	ctx := context.Background()
	hbTime := time.Now().Add(5 * time.Minute)
	hb := &loka.Heartbeat{
		WorkerID:  w.ID,
		Timestamp: hbTime,
		Status:    loka.WorkerStatusBusy,
	}
	err := r.UpdateHeartbeat(ctx, w.ID, hb)
	if err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}

	conn, _ := r.Get(w.ID)
	if conn.Worker.Status != loka.WorkerStatusBusy {
		t.Errorf("Status after heartbeat = %q, want %q", conn.Worker.Status, loka.WorkerStatusBusy)
	}
	if !conn.Worker.LastSeen.Equal(hbTime) {
		t.Errorf("LastSeen after heartbeat = %v, want %v", conn.Worker.LastSeen, hbTime)
	}
}

func TestUpdateHeartbeat_NotFound(t *testing.T) {
	r := newTestRegistry()
	ctx := context.Background()
	err := r.UpdateHeartbeat(ctx, "nonexistent", &loka.Heartbeat{})
	if err == nil {
		t.Fatal("expected error for unknown worker, got nil")
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := newTestRegistry()
	ctx := context.Background()

	var wg sync.WaitGroup
	// Concurrently register workers.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Register(ctx, "host", "10.0.0.1", "aws", "us-east-1", "zone", "v1",
				loka.ResourceCapacity{}, nil, true)
			if err != nil {
				t.Errorf("concurrent Register: %v", err)
			}
		}()
	}
	// Concurrently list workers.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.List()
		}()
	}
	wg.Wait()

	conns := r.List()
	if len(conns) != 20 {
		t.Errorf("after concurrent ops: got %d workers, want 20", len(conns))
	}
}
