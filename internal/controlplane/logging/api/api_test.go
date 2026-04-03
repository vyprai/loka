package logapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/logging"
	"github.com/vyprai/loka/internal/controlplane/logging/store"
)

// mockLogStore implements store.LogStore for testing.
type mockLogStore struct {
	labels      []string
	labelValues map[string][]string
	streams     map[uint64]map[string]string // streamID -> labels
	entries     []logging.LogEntry
}

func newMockLogStore() *mockLogStore {
	return &mockLogStore{
		labels:      []string{"type", "source"},
		labelValues: map[string][]string{"type": {"service", "task"}, "source": {"cp", "worker"}},
		streams:     make(map[uint64]map[string]string),
		entries:     nil,
	}
}

func (m *mockLogStore) Write(_ context.Context, entries []logging.LogEntry) error {
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *mockLogStore) Query(_ context.Context, _ logging.QueryRequest) (*logging.QueryResult, error) {
	return &logging.QueryResult{Streams: []logging.Stream{}}, nil
}

func (m *mockLogStore) QueryWithMatchers(_ context.Context, streamIDs []uint64, start, end time.Time, limit int, direction string, lineFilter func(string) bool) (*logging.QueryResult, error) {
	var streams []logging.Stream
	for _, sid := range streamIDs {
		labels, ok := m.streams[sid]
		if !ok {
			continue
		}
		var matched []logging.LogEntry
		for _, e := range m.entries {
			if e.StreamID() != sid {
				continue
			}
			if e.Timestamp.Before(start) || e.Timestamp.After(end) {
				continue
			}
			if lineFilter != nil && !lineFilter(e.Message) {
				continue
			}
			matched = append(matched, e)
			if len(matched) >= limit {
				break
			}
		}
		if len(matched) > 0 {
			streams = append(streams, logging.Stream{Labels: labels, Entries: matched})
		}
	}
	return &logging.QueryResult{Streams: streams}, nil
}

func (m *mockLogStore) Tail(_ context.Context, _ string) (<-chan logging.LogEntry, error) {
	ch := make(chan logging.LogEntry)
	close(ch)
	return ch, nil
}

func (m *mockLogStore) ListLabels(_ context.Context) ([]string, error) {
	return m.labels, nil
}

func (m *mockLogStore) ListLabelValues(_ context.Context, labelName string) ([]string, error) {
	vals, ok := m.labelValues[labelName]
	if !ok {
		return []string{}, nil
	}
	return vals, nil
}

func (m *mockLogStore) FindStreams(_ context.Context, matchers []store.LabelMatcher) ([]uint64, error) {
	var ids []uint64
	for sid, labels := range m.streams {
		allMatch := true
		for _, mat := range matchers {
			val, exists := labels[mat.Name]
			switch mat.Type {
			case store.MatchEqual:
				if val != mat.Value {
					allMatch = false
				}
			case store.MatchNotEqual:
				if val == mat.Value {
					allMatch = false
				}
			default:
				if !exists {
					allMatch = false
				}
			}
		}
		if allMatch {
			ids = append(ids, sid)
		}
	}
	return ids, nil
}

func (m *mockLogStore) GetStreamLabels(_ context.Context, streamID uint64) (map[string]string, error) {
	labels, ok := m.streams[streamID]
	if !ok {
		return nil, nil
	}
	return labels, nil
}

func (m *mockLogStore) Close() error { return nil }

// seedEntry adds an entry to the mock store and registers its stream.
func (m *mockLogStore) seedEntry(ts time.Time, msg string, labels map[string]string) {
	e := logging.LogEntry{Timestamp: ts, Level: "info", Message: msg, Labels: labels}
	m.entries = append(m.entries, e)
	m.streams[e.StreamID()] = labels
}

// newTestRouter creates a chi router mounted with LogsAPI routes.
func newTestRouter(ms *mockLogStore) http.Handler {
	api := &LogsAPI{Store: ms}
	r := chi.NewRouter()
	r.Mount("/loki/api/v1", api.Routes())
	return r
}

func TestQueryRange(t *testing.T) {
	ms := newMockLogStore()
	now := time.Now()
	ms.seedEntry(now.Add(-10*time.Minute), "hello world", map[string]string{"type": "service"})
	router := newTestRouter(ms)

	start := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	end := now.UTC().Format(time.RFC3339)
	req := httptest.NewRequest("GET", "/loki/api/v1/query_range?query={type=\"service\"}&start="+start+"&end="+end+"&limit=10", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp lokiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected success, got %s", resp.Status)
	}
}

func TestQueryInstant(t *testing.T) {
	ms := newMockLogStore()
	now := time.Now()
	ms.seedEntry(now.Add(-30*time.Second), "instant entry", map[string]string{"type": "service"})
	router := newTestRouter(ms)

	req := httptest.NewRequest("GET", "/loki/api/v1/query?query={type=\"service\"}&limit=5", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp lokiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected success, got %s", resp.Status)
	}
}

func TestLabels(t *testing.T) {
	ms := newMockLogStore()
	router := newTestRouter(ms)

	req := httptest.NewRequest("GET", "/loki/api/v1/labels", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected success, got %s", resp.Status)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(resp.Data))
	}
}

func TestLabelValues(t *testing.T) {
	ms := newMockLogStore()
	router := newTestRouter(ms)

	req := httptest.NewRequest("GET", "/loki/api/v1/label/type/values", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 values, got %d", len(resp.Data))
	}
}

func TestSeries(t *testing.T) {
	ms := newMockLogStore()
	ms.streams[12345] = map[string]string{"type": "service", "name": "web"}
	router := newTestRouter(ms)

	req := httptest.NewRequest("GET", `/loki/api/v1/series?match[]={type="service"}`, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp lokiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected success, got %s", resp.Status)
	}
}

func TestQueryMissingParam(t *testing.T) {
	ms := newMockLogStore()
	router := newTestRouter(ms)

	req := httptest.NewRequest("GET", "/loki/api/v1/query_range", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var resp lokiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestQueryEmptyResult(t *testing.T) {
	ms := newMockLogStore()
	// No entries seeded — FindStreams returns empty.
	router := newTestRouter(ms)

	req := httptest.NewRequest("GET", `/loki/api/v1/query?query={type="nonexistent"}&limit=5`, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp lokiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected success, got %s", resp.Status)
	}
}
