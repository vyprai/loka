package recorder

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
)

// mockStore implements tsdb.MetricsStore for testing (only Write is used).
type mockStore struct {
	mu     sync.Mutex
	points []metrics.DataPoint
}

func (m *mockStore) Write(_ context.Context, points []metrics.DataPoint) error {
	m.mu.Lock()
	m.points = append(m.points, points...)
	m.mu.Unlock()
	return nil
}

func (m *mockStore) Query(_ context.Context, _ tsdb.QueryRequest) ([]tsdb.QueryResult, error) {
	return nil, nil
}
func (m *mockStore) ListMetrics(context.Context) ([]string, error)    { return nil, nil }
func (m *mockStore) ListLabelNames(context.Context) ([]string, error) { return nil, nil }
func (m *mockStore) ListLabelValues(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (m *mockStore) FindSeries(_ context.Context, _ []tsdb.LabelMatcher) ([]uint64, error) {
	return nil, nil
}
func (m *mockStore) GetSeriesInfo(_ context.Context, _ uint64) (*metrics.SeriesInfo, error) {
	return nil, nil
}
func (m *mockStore) GetStats() *tsdb.Stats   { return &tsdb.Stats{} }
func (m *mockStore) DiskSize() (int64, int64) { return 0, 0 }
func (m *mockStore) Close() error             { return nil }

func (m *mockStore) getPoints() []metrics.DataPoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]metrics.DataPoint, len(m.points))
	copy(cp, m.points)
	return cp
}

func TestRecorderInc(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)
	defer stop()

	rec.Inc("requests_total", metrics.Label{Name: "method", Value: "GET"})

	// Force flush via stop.
	stop()

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point, got %d", len(pts))
	}
	if pts[0].Name != "requests_total" {
		t.Errorf("expected name requests_total, got %s", pts[0].Name)
	}
	if pts[0].Type != metrics.Counter {
		t.Errorf("expected Counter type, got %v", pts[0].Type)
	}
	if pts[0].Value != 1 {
		t.Errorf("expected value 1, got %f", pts[0].Value)
	}
	if len(pts[0].Labels) != 1 || pts[0].Labels[0].Name != "method" {
		t.Errorf("unexpected labels: %v", pts[0].Labels)
	}
}

func TestRecorderAdd(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)

	rec.Add("bytes_total", 42.5)
	stop()

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point, got %d", len(pts))
	}
	if pts[0].Type != metrics.Counter {
		t.Errorf("expected Counter, got %v", pts[0].Type)
	}
	if pts[0].Value != 42.5 {
		t.Errorf("expected 42.5, got %f", pts[0].Value)
	}
}

func TestRecorderSet(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)

	rec.Set("goroutines", 100)
	stop()

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point, got %d", len(pts))
	}
	if pts[0].Type != metrics.Gauge {
		t.Errorf("expected Gauge, got %v", pts[0].Type)
	}
	if pts[0].Value != 100 {
		t.Errorf("expected 100, got %f", pts[0].Value)
	}
}

func TestRecorderObserve(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)

	rec.Observe("latency_seconds", 0.25)
	stop()

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point, got %d", len(pts))
	}
	if pts[0].Type != metrics.Histogram {
		t.Errorf("expected Histogram, got %v", pts[0].Type)
	}
	if pts[0].Value != 0.25 {
		t.Errorf("expected 0.25, got %f", pts[0].Value)
	}
}

func TestRecorderTimerFlush(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)
	defer stop()

	rec.Inc("timer_test")

	// Wait for the 1s ticker to flush.
	time.Sleep(1500 * time.Millisecond)

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point after timer flush, got %d", len(pts))
	}
}

func TestRecorderBufferOverflowFlush(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)
	defer stop()

	// Fill buffer beyond capacity to trigger an immediate flush.
	for i := 0; i < bufferCapacity+10; i++ {
		rec.Inc("overflow_test")
	}

	// Give a moment for the flush triggered by overflow to complete.
	time.Sleep(50 * time.Millisecond)

	pts := store.getPoints()
	if len(pts) < bufferCapacity {
		t.Fatalf("expected at least %d points flushed on overflow, got %d", bufferCapacity, len(pts))
	}
}

func TestRecorderConcurrent(t *testing.T) {
	store := &mockStore{}
	rec, stop := New(context.Background(), store)

	var wg sync.WaitGroup
	n := 100
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			rec.Inc("concurrent_test")
		}()
	}
	wg.Wait()
	stop()

	pts := store.getPoints()
	if len(pts) != n {
		t.Fatalf("expected %d points, got %d", n, len(pts))
	}
}

func TestNewWithNilStoreReturnsNop(t *testing.T) {
	rec, stop := New(context.Background(), nil)
	defer stop()

	// Should not panic.
	rec.Inc("nop_test")
	rec.Add("nop_test", 1)
	rec.Set("nop_test", 1)
	rec.Observe("nop_test", 1)

	if _, ok := rec.(NopRecorder); !ok {
		t.Fatal("expected NopRecorder when store is nil")
	}
}

func TestNopRecorder(t *testing.T) {
	var nop NopRecorder
	// Just verify these don't panic.
	nop.Inc("test")
	nop.Add("test", 1)
	nop.Set("test", 1)
	nop.Observe("test", 1)
}

func TestNopRecorderAllMethods(t *testing.T) {
	var nop NopRecorder

	// Verify all methods work without panic, including with labels.
	nop.Inc("counter_a")
	nop.Inc("counter_b", metrics.Label{Name: "method", Value: "GET"})
	nop.Inc("counter_c", metrics.Label{Name: "method", Value: "POST"}, metrics.Label{Name: "status", Value: "200"})

	nop.Add("bytes_total", 0)
	nop.Add("bytes_total", 100.5, metrics.Label{Name: "dir", Value: "in"})

	nop.Set("goroutines", 0)
	nop.Set("goroutines", -1, metrics.Label{Name: "pool", Value: "worker"})

	nop.Observe("latency", 0)
	nop.Observe("latency", 0.001, metrics.Label{Name: "endpoint", Value: "/health"})

	// Verify it satisfies the Recorder interface.
	var _ Recorder = nop
}

func TestRecorderContextCancellation(t *testing.T) {
	store := &mockStore{}
	ctx, cancel := context.WithCancel(context.Background())
	rec, stop := New(ctx, store)

	rec.Inc("cancel_test")

	// Cancel parent context.
	cancel()
	// stop waits for flushLoop to exit (which does a final flush).
	stop()

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point after cancellation flush, got %d", len(pts))
	}
}

func TestRecorderTimestamp(t *testing.T) {
	store := &mockStore{}
	before := time.Now().UnixMilli()
	rec, stop := New(context.Background(), store)
	rec.Inc("ts_test")
	stop()
	after := time.Now().UnixMilli()

	pts := store.getPoints()
	if len(pts) != 1 {
		t.Fatalf("expected 1 point, got %d", len(pts))
	}
	if pts[0].Timestamp < before || pts[0].Timestamp > after {
		t.Errorf("timestamp %d not in range [%d, %d]", pts[0].Timestamp, before, after)
	}
}
