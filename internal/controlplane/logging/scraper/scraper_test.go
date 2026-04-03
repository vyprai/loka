package scraper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/logging"
	"github.com/vyprai/loka/internal/controlplane/logging/store"
)

// mockLogStore implements store.LogStore for scraper tests.
type mockLogStore struct {
	mu      sync.Mutex
	written []logging.LogEntry
}

func (m *mockLogStore) Write(_ context.Context, entries []logging.LogEntry) error {
	m.mu.Lock()
	m.written = append(m.written, entries...)
	m.mu.Unlock()
	return nil
}

func (m *mockLogStore) Query(_ context.Context, _ logging.QueryRequest) (*logging.QueryResult, error) {
	return &logging.QueryResult{}, nil
}

func (m *mockLogStore) QueryWithMatchers(_ context.Context, _ []uint64, _, _ time.Time, _ int, _ string, _ func(string) bool) (*logging.QueryResult, error) {
	return &logging.QueryResult{}, nil
}

func (m *mockLogStore) Tail(_ context.Context, _ string) (<-chan logging.LogEntry, error) {
	ch := make(chan logging.LogEntry)
	close(ch)
	return ch, nil
}

func (m *mockLogStore) ListLabels(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockLogStore) ListLabelValues(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockLogStore) FindStreams(_ context.Context, _ []store.LabelMatcher) ([]uint64, error) {
	return nil, nil
}

func (m *mockLogStore) GetStreamLabels(_ context.Context, _ uint64) (map[string]string, error) {
	return nil, nil
}

func (m *mockLogStore) Close() error { return nil }

func (m *mockLogStore) getWritten() []logging.LogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]logging.LogEntry, len(m.written))
	copy(cp, m.written)
	return cp
}

// staticDiscovery returns a fixed set of targets.
type staticDiscovery struct {
	targets []ScrapeTarget
}

func (d *staticDiscovery) Targets(_ context.Context) ([]ScrapeTarget, error) {
	return d.targets, nil
}

func TestScrapeFromWorker(t *testing.T) {
	now := time.Now()
	entries := []workerEntry{
		{Timestamp: now, Level: "info", Message: "hello from worker", Labels: map[string]string{"type": "service"}},
		{Timestamp: now.Add(time.Second), Level: "warn", Message: "something happened", Labels: map[string]string{"type": "task"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"entries": entries,
			"count":   len(entries),
		})
	}))
	defer srv.Close()

	ms := &mockLogStore{}
	discovery := &staticDiscovery{
		targets: []ScrapeTarget{{Address: srv.Listener.Addr().String(), Type: "worker", Labels: map[string]string{"worker_id": "w1"}}},
	}
	scraper := New(ms, discovery, time.Hour, nil) // long interval so run loop doesn't interfere

	ctx := context.Background()
	scraper.scrapeAll(ctx)

	written := ms.getWritten()
	if len(written) != 2 {
		t.Fatalf("expected 2 entries written, got %d", len(written))
	}
	if written[0].Message != "hello from worker" {
		t.Fatalf("unexpected message: %s", written[0].Message)
	}
	// Verify target labels are merged.
	if written[0].Labels["worker_id"] != "w1" {
		t.Fatalf("expected worker_id=w1, got %s", written[0].Labels["worker_id"])
	}
}

func TestScrapeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := &mockLogStore{}
	discovery := &staticDiscovery{
		targets: []ScrapeTarget{{Address: srv.Listener.Addr().String(), Type: "worker"}},
	}
	scraper := New(ms, discovery, time.Hour, nil)

	ctx := context.Background()
	scraper.scrapeAll(ctx)

	written := ms.getWritten()
	if len(written) != 0 {
		t.Fatalf("expected 0 entries on failure, got %d", len(written))
	}
}

func TestScrapeStartStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"entries": []workerEntry{}, "count": 0})
	}))
	defer srv.Close()

	ms := &mockLogStore{}
	discovery := &staticDiscovery{
		targets: []ScrapeTarget{{Address: srv.Listener.Addr().String(), Type: "worker"}},
	}
	scraper := New(ms, discovery, 50*time.Millisecond, nil)

	scraper.Start()
	time.Sleep(100 * time.Millisecond)
	scraper.Stop()
	// No panic or deadlock means the lifecycle works.
}

func TestScrapeCursorAdvancement(t *testing.T) {
	now := time.Now()
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			entries := []workerEntry{
				{Timestamp: now, Level: "info", Message: "first", Labels: map[string]string{"type": "service"}},
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries, "count": 1})
		} else {
			entries := []workerEntry{
				{Timestamp: now.Add(time.Second), Level: "info", Message: "second", Labels: map[string]string{"type": "service"}},
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries, "count": 1})
		}
	}))
	defer srv.Close()

	ms := &mockLogStore{}
	addr := srv.Listener.Addr().String()
	discovery := &staticDiscovery{
		targets: []ScrapeTarget{{Address: addr, Type: "worker"}},
	}
	scraper := New(ms, discovery, time.Hour, nil)

	ctx := context.Background()
	scraper.scrapeAll(ctx)
	scraper.scrapeAll(ctx)

	written := ms.getWritten()
	if len(written) != 2 {
		t.Fatalf("expected 2 entries across 2 scrapes, got %d", len(written))
	}
	if written[1].Message != "second" {
		t.Fatalf("expected second entry from second scrape, got %s", written[1].Message)
	}

	// Verify cursor advanced past the first entry's timestamp.
	scraper.mu.RLock()
	cursor := scraper.lastCursor[addr]
	scraper.mu.RUnlock()
	if !cursor.After(now) {
		t.Fatalf("cursor should have advanced past first entry timestamp")
	}
}

func TestScrapeWritesToStore(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries := []workerEntry{
			{Timestamp: now, Level: "error", Message: "disk full", Labels: map[string]string{"type": "system"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries, "count": 1})
	}))
	defer srv.Close()

	ms := &mockLogStore{}
	discovery := &staticDiscovery{
		targets: []ScrapeTarget{{Address: srv.Listener.Addr().String(), Type: "worker", Labels: map[string]string{"env": "prod"}}},
	}
	scraper := New(ms, discovery, time.Hour, nil)

	ctx := context.Background()
	scraper.scrapeAll(ctx)

	written := ms.getWritten()
	if len(written) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(written))
	}
	if written[0].Level != "error" {
		t.Fatalf("expected level=error, got %s", written[0].Level)
	}
	if written[0].Labels["env"] != "prod" {
		t.Fatalf("expected env=prod label from target, got %s", written[0].Labels["env"])
	}
	if written[0].Labels["type"] != "system" {
		t.Fatalf("expected type=system from entry, got %s", written[0].Labels["type"])
	}
}
