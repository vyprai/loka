package scraper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
)

// mockStore implements tsdb.MetricsStore for testing.
type mockStore struct {
	written []metrics.DataPoint
}

func (m *mockStore) Write(_ context.Context, points []metrics.DataPoint) error {
	m.written = append(m.written, points...)
	return nil
}

func (m *mockStore) Query(_ context.Context, _ tsdb.QueryRequest) ([]tsdb.QueryResult, error) {
	return nil, nil
}

func (m *mockStore) ListMetrics(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockStore) ListLabelNames(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockStore) ListLabelValues(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockStore) FindSeries(_ context.Context, _ []tsdb.LabelMatcher) ([]uint64, error) {
	return nil, nil
}

func (m *mockStore) GetSeriesInfo(_ context.Context, _ uint64) (*metrics.SeriesInfo, error) {
	return nil, nil
}

func (m *mockStore) GetStats() *tsdb.Stats {
	return &tsdb.Stats{}
}

func (m *mockStore) DiskSize() (int64, int64) {
	return 0, 0
}

func (m *mockStore) Close() error {
	return nil
}

func TestParsePrometheusText_Basic(t *testing.T) {
	input := `# HELP http_requests_total Total HTTP requests.
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 1027
http_requests_total{method="POST",code="200"} 42

# HELP temperature_celsius Current temperature.
# TYPE temperature_celsius gauge
temperature_celsius{location="outside"} 21.3

# HELP request_duration_seconds Request duration histogram.
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="0.05"} 24054
request_duration_seconds_bucket{le="0.1"} 33444
request_duration_seconds_bucket{le="+Inf"} 144320
request_duration_seconds_sum 53423
request_duration_seconds_count 144320
`
	r := strings.NewReader(input)
	points, err := parsePrometheusText(r, nil, 1000)
	if err != nil {
		t.Fatalf("parsePrometheusText: %v", err)
	}

	if len(points) != 8 {
		t.Fatalf("expected 8 data points, got %d", len(points))
	}

	// Verify counter type.
	for _, dp := range points {
		if dp.Name == "http_requests_total" && dp.Type != metrics.Counter {
			t.Errorf("http_requests_total type = %v, want Counter", dp.Type)
		}
	}

	// Verify gauge type.
	for _, dp := range points {
		if dp.Name == "temperature_celsius" && dp.Type != metrics.Gauge {
			t.Errorf("temperature_celsius type = %v, want Gauge", dp.Type)
		}
	}

	// Verify histogram type on bucket/sum/count lines.
	for _, dp := range points {
		if strings.HasPrefix(dp.Name, "request_duration_seconds") && dp.Type != metrics.Histogram {
			t.Errorf("%s type = %v, want Histogram", dp.Name, dp.Type)
		}
	}
}

func TestParseMetricLine_WithLabels(t *testing.T) {
	line := `metric{label1="val1",label2="val2"} 42.5`
	dp, err := parseMetricLine(line, nil, nil, 1000)
	if err != nil {
		t.Fatalf("parseMetricLine: %v", err)
	}
	if dp.Name != "metric" {
		t.Errorf("Name = %q, want %q", dp.Name, "metric")
	}
	if dp.Value != 42.5 {
		t.Errorf("Value = %f, want 42.5", dp.Value)
	}
	if len(dp.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(dp.Labels))
	}
	lm := dp.Labels.Map()
	if lm["label1"] != "val1" {
		t.Errorf("label1 = %q, want %q", lm["label1"], "val1")
	}
	if lm["label2"] != "val2" {
		t.Errorf("label2 = %q, want %q", lm["label2"], "val2")
	}
}

func TestParseMetricLine_NoLabels(t *testing.T) {
	line := `metric 42.5`
	dp, err := parseMetricLine(line, nil, nil, 1000)
	if err != nil {
		t.Fatalf("parseMetricLine: %v", err)
	}
	if dp.Name != "metric" {
		t.Errorf("Name = %q, want %q", dp.Name, "metric")
	}
	if dp.Value != 42.5 {
		t.Errorf("Value = %f, want 42.5", dp.Value)
	}
	if len(dp.Labels) != 0 {
		t.Errorf("expected 0 labels, got %d", len(dp.Labels))
	}
	if dp.Timestamp != 1000 {
		t.Errorf("Timestamp = %d, want 1000", dp.Timestamp)
	}
}

func TestParseMetricLine_WithTimestamp(t *testing.T) {
	line := `metric 42.5 1234567890`
	dp, err := parseMetricLine(line, nil, nil, 9999)
	if err != nil {
		t.Fatalf("parseMetricLine: %v", err)
	}
	if dp.Value != 42.5 {
		t.Errorf("Value = %f, want 42.5", dp.Value)
	}
	if dp.Timestamp != 1234567890 {
		t.Errorf("Timestamp = %d, want 1234567890", dp.Timestamp)
	}
}

func TestParseLabels_EscapedChars(t *testing.T) {
	// Escaped double quote, backslash, and newline in label values.
	s := `path="foo\\bar",msg="hello \"world\"",nl="line1\nline2"`
	labels := parseLabels(s)

	lm := make(map[string]string)
	for _, l := range labels {
		lm[l.Name] = l.Value
	}

	if lm["path"] != `foo\bar` {
		t.Errorf("path = %q, want %q", lm["path"], `foo\bar`)
	}
	if lm["msg"] != `hello "world"` {
		t.Errorf("msg = %q, want %q", lm["msg"], `hello "world"`)
	}
	if lm["nl"] != "line1\nline2" {
		t.Errorf("nl = %q, want %q", lm["nl"], "line1\nline2")
	}
}

func TestParseMetricLine_TypeInference(t *testing.T) {
	tests := []struct {
		line     string
		wantType metrics.MetricType
	}{
		{`http_requests_total 100`, metrics.Counter},
		{`request_duration_seconds_bucket{le="0.5"} 42`, metrics.Gauge}, // no type map entry, no _total suffix
	}

	for _, tt := range tests {
		dp, err := parseMetricLine(tt.line, nil, nil, 1000)
		if err != nil {
			t.Fatalf("parseMetricLine(%q): %v", tt.line, err)
		}
		if dp.Type != tt.wantType {
			t.Errorf("parseMetricLine(%q).Type = %v, want %v", tt.line, dp.Type, tt.wantType)
		}
	}

	// With type map: _bucket resolves to histogram via base name.
	typeMap := map[string]metrics.MetricType{
		"request_duration_seconds": metrics.Histogram,
	}
	dp, err := parseMetricLine(`request_duration_seconds_bucket{le="0.5"} 42`, typeMap, nil, 1000)
	if err != nil {
		t.Fatalf("parseMetricLine with typeMap: %v", err)
	}
	if dp.Type != metrics.Histogram {
		t.Errorf("with typeMap, Type = %v, want Histogram", dp.Type)
	}
}

func TestParseMetricLine_ExtraLabels(t *testing.T) {
	line := `metric{env="prod"} 10`
	extra := map[string]string{
		"instance": "node-1",
		"job":      "api",
	}
	dp, err := parseMetricLine(line, nil, extra, 1000)
	if err != nil {
		t.Fatalf("parseMetricLine: %v", err)
	}

	lm := dp.Labels.Map()
	if lm["env"] != "prod" {
		t.Errorf("env = %q, want %q", lm["env"], "prod")
	}
	if lm["instance"] != "node-1" {
		t.Errorf("instance = %q, want %q", lm["instance"], "node-1")
	}
	if lm["job"] != "api" {
		t.Errorf("job = %q, want %q", lm["job"], "api")
	}
	if len(dp.Labels) != 3 {
		t.Errorf("expected 3 labels, got %d", len(dp.Labels))
	}

	// Verify labels are sorted.
	for i := 1; i < len(dp.Labels); i++ {
		if dp.Labels[i-1].Name >= dp.Labels[i].Name {
			t.Errorf("labels not sorted: %q >= %q", dp.Labels[i-1].Name, dp.Labels[i].Name)
		}
	}
}

func TestScrapeTarget_MockServer(t *testing.T) {
	body := `# HELP up Whether the target is up.
# TYPE up gauge
up 1
# HELP http_requests_total Total requests.
# TYPE http_requests_total counter
http_requests_total{method="GET"} 100
http_requests_total{method="POST"} 25
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	store := &mockStore{}
	target := ScrapeTarget{
		Address: strings.TrimPrefix(srv.URL, "http://"),
		Type:    "worker",
		Labels:  map[string]string{"job": "test"},
	}

	s := New(store, nil, 0, nil)
	s.httpClient = srv.Client()
	s.scrapeTarget(context.Background(), target)

	// The store should have the scraped metrics + 3 scrape_* meta-metrics.
	// Scraped metrics: up, http_requests_total (GET), http_requests_total (POST) = 3
	// Meta-metrics: scrape_up, scrape_duration_seconds, scrape_samples_scraped = 3
	// Total = 6
	if len(store.written) != 6 {
		t.Fatalf("expected 6 written points, got %d", len(store.written))
	}

	// Verify scraped metrics have the extra job label.
	for _, dp := range store.written[:3] {
		if dp.Labels.Get("job") != "test" {
			t.Errorf("metric %q missing job=test label", dp.Name)
		}
	}

	// Verify up metric value.
	found := false
	for _, dp := range store.written {
		if dp.Name == "up" {
			if dp.Value != 1.0 {
				t.Errorf("up value = %f, want 1.0", dp.Value)
			}
			if dp.Type != metrics.Gauge {
				t.Errorf("up type = %v, want Gauge", dp.Type)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("up metric not found in written points")
	}

	// Verify scrape_up meta-metric.
	for _, dp := range store.written {
		if dp.Name == "scrape_up" {
			if dp.Value != 1.0 {
				t.Errorf("scrape_up = %f, want 1.0", dp.Value)
			}
			break
		}
	}
}
