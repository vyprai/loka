package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
)

// mockStore implements tsdb.MetricsStore for testing.
type mockStore struct {
	series    map[uint64]*metrics.SeriesInfo
	points    map[uint64][]tsdb.TimeValue
	metrics   []string
	labelNames []string
	labelValues map[string][]string
}

func newMockStore() *mockStore {
	return &mockStore{
		series:      make(map[uint64]*metrics.SeriesInfo),
		points:      make(map[uint64][]tsdb.TimeValue),
		labelValues: make(map[string][]string),
	}
}

func (m *mockStore) Write(_ context.Context, _ []metrics.DataPoint) error { return nil }

func (m *mockStore) Query(_ context.Context, req tsdb.QueryRequest) ([]tsdb.QueryResult, error) {
	var results []tsdb.QueryResult
	for _, sid := range req.SeriesIDs {
		pts, ok := m.points[sid]
		if !ok {
			continue
		}
		var filtered []tsdb.TimeValue
		startMs := req.Start.UnixMilli()
		endMs := req.End.UnixMilli()
		for _, p := range pts {
			if p.TimestampMs >= startMs && p.TimestampMs <= endMs {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) > 0 {
			results = append(results, tsdb.QueryResult{SeriesID: sid, Points: filtered})
		}
	}
	return results, nil
}

func (m *mockStore) ListMetrics(_ context.Context) ([]string, error) {
	return m.metrics, nil
}

func (m *mockStore) ListLabelNames(_ context.Context) ([]string, error) {
	return m.labelNames, nil
}

func (m *mockStore) ListLabelValues(_ context.Context, labelName string) ([]string, error) {
	return m.labelValues[labelName], nil
}

func (m *mockStore) FindSeries(_ context.Context, matchers []tsdb.LabelMatcher) ([]uint64, error) {
	var ids []uint64
	for sid, info := range m.series {
		if matchesAll(info, matchers) {
			ids = append(ids, sid)
		}
	}
	return ids, nil
}

func matchesAll(info *metrics.SeriesInfo, matchers []tsdb.LabelMatcher) bool {
	for _, mat := range matchers {
		var val string
		if mat.Name == "__name__" {
			val = info.Name
		} else {
			val = info.Labels[mat.Name]
		}
		switch mat.Type {
		case tsdb.MatchEqual:
			if val != mat.Value {
				return false
			}
		case tsdb.MatchNotEqual:
			if val == mat.Value {
				return false
			}
		}
	}
	return true
}

func (m *mockStore) GetSeriesInfo(_ context.Context, seriesID uint64) (*metrics.SeriesInfo, error) {
	info, ok := m.series[seriesID]
	if !ok {
		return nil, context.Canceled
	}
	return info, nil
}

func (m *mockStore) GetStats() *tsdb.Stats {
	return &tsdb.Stats{}
}

func (m *mockStore) DiskSize() (int64, int64) {
	return 0, 0
}

func (m *mockStore) Close() error { return nil }

// addSeries is a helper to add a series with points to the mock store.
func (m *mockStore) addSeries(id uint64, name string, labels map[string]string, points []tsdb.TimeValue) {
	m.series[id] = &metrics.SeriesInfo{Name: name, Labels: labels}
	m.points[id] = points
}

// setupRouter creates a chi router with the MetricsAPI mounted.
func setupRouter(store tsdb.MetricsStore) *chi.Mux {
	api := &MetricsAPI{Store: store}
	r := chi.NewRouter()
	r.Mount("/api/v1", api.Routes())
	return r
}

func TestInstantQuery_Selector(t *testing.T) {
	store := newMockStore()
	now := time.Now()
	nowMs := now.UnixMilli()

	store.addSeries(1, "cpu", map[string]string{"type": "service"}, []tsdb.TimeValue{
		{TimestampMs: nowMs - 1000, Value: 42.5},
	})
	store.addSeries(2, "cpu", map[string]string{"type": "worker"}, []tsdb.TimeValue{
		{TimestampMs: nowMs - 1000, Value: 80.0},
	})

	r := setupRouter(store)
	req := httptest.NewRequest("GET", `/api/v1/query?query=cpu{type="service"}`, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}

	data, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	var vd vectorData
	if err := json.Unmarshal(data, &vd); err != nil {
		t.Fatalf("failed to decode vector data: %v", err)
	}
	if vd.ResultType != "vector" {
		t.Fatalf("expected resultType vector, got %s", vd.ResultType)
	}
	if len(vd.Result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(vd.Result))
	}
	if vd.Result[0].Metric["type"] != "service" {
		t.Errorf("expected label type=service, got %v", vd.Result[0].Metric)
	}
}

func TestRangeQuery(t *testing.T) {
	store := newMockStore()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	store.addSeries(1, "cpu", map[string]string{}, []tsdb.TimeValue{
		{TimestampMs: base.UnixMilli(), Value: 10},
		{TimestampMs: base.Add(1 * time.Minute).UnixMilli(), Value: 20},
		{TimestampMs: base.Add(2 * time.Minute).UnixMilli(), Value: 30},
	})

	r := setupRouter(store)
	start := base.Format(time.RFC3339)
	end := base.Add(2 * time.Minute).Format(time.RFC3339)
	url := "/api/v1/query_range?query=cpu&start=" + start + "&end=" + end + "&step=1m"
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}

	data, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	var md matrixData
	if err := json.Unmarshal(data, &md); err != nil {
		t.Fatalf("failed to decode matrix data: %v", err)
	}
	if md.ResultType != "matrix" {
		t.Fatalf("expected resultType matrix, got %s", md.ResultType)
	}
	if len(md.Result) == 0 {
		t.Fatal("expected at least 1 matrix result")
	}
	if len(md.Result[0].Values) == 0 {
		t.Fatal("expected matrix result to have values")
	}
}

func TestSeriesEndpoint(t *testing.T) {
	store := newMockStore()
	now := time.Now()
	store.addSeries(1, "cpu", map[string]string{"type": "service"}, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 1},
	})

	r := setupRouter(store)
	req := httptest.NewRequest("GET", `/api/v1/series?match[]=cpu{type="service"}`, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}

	data, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	var labelSets []map[string]string
	if err := json.Unmarshal(data, &labelSets); err != nil {
		t.Fatalf("failed to decode label sets: %v", err)
	}
	if len(labelSets) != 1 {
		t.Fatalf("expected 1 label set, got %d", len(labelSets))
	}
	if labelSets[0]["__name__"] != "cpu" {
		t.Errorf("expected __name__=cpu, got %v", labelSets[0])
	}
	if labelSets[0]["type"] != "service" {
		t.Errorf("expected type=service, got %v", labelSets[0])
	}
}

func TestLabelsEndpoint(t *testing.T) {
	store := newMockStore()
	store.labelNames = []string{"__name__", "type", "region"}

	r := setupRouter(store)
	req := httptest.NewRequest("GET", "/api/v1/labels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}

	data, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		t.Fatalf("failed to decode names: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 label names, got %d", len(names))
	}
}

func TestLabelValuesEndpoint(t *testing.T) {
	store := newMockStore()
	store.labelValues["type"] = []string{"service", "worker", "task"}

	r := setupRouter(store)
	req := httptest.NewRequest("GET", "/api/v1/label/type/values", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %s", resp.Status)
	}

	data, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatalf("failed to decode values: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 values, got %d", len(values))
	}
}

func TestQueryMissingParam(t *testing.T) {
	store := newMockStore()
	r := setupRouter(store)

	req := httptest.NewRequest("GET", "/api/v1/query", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "error" {
		t.Fatalf("expected status error, got %s", resp.Status)
	}
	if resp.ErrorType != "bad_data" {
		t.Errorf("expected errorType bad_data, got %s", resp.ErrorType)
	}
}

func TestQueryBadExpression(t *testing.T) {
	store := newMockStore()
	r := setupRouter(store)

	req := httptest.NewRequest("GET", "/api/v1/query?query=!!!invalid", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}

	var resp promResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "error" {
		t.Fatalf("expected status error, got %s", resp.Status)
	}
	if resp.ErrorType != "bad_data" {
		t.Errorf("expected errorType bad_data, got %s", resp.ErrorType)
	}
}
