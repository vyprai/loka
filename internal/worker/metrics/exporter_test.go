package workermetrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/metrics"
)

// testScraper is a Scraper with manually set points (no background goroutine).
func newTestScraper(points []metrics.DataPoint) *Scraper {
	s := &Scraper{
		sessions: make(map[string]*SessionInfo),
		points:   points,
		cancel:   make(chan struct{}),
	}
	return s
}

func TestExporterPrometheusFormat(t *testing.T) {
	points := []metrics.DataPoint{
		{
			Name:   "cpu_usage",
			Type:   metrics.Gauge,
			Labels: metrics.Labels{{Name: "id", Value: "svc_1"}, {Name: "type", Value: "service"}},
			Value:  42.5,
		},
		{
			Name:   "requests_total",
			Type:   metrics.Counter,
			Labels: metrics.Labels{{Name: "id", Value: "svc_1"}, {Name: "type", Value: "service"}},
			Value:  100,
		},
	}

	scraper := newTestScraper(points)
	exporter := NewExporter(scraper)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	exporter.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()

	if !strings.Contains(body, "# TYPE cpu_usage gauge") {
		t.Error("missing TYPE comment for cpu_usage")
	}
	if !strings.Contains(body, "# TYPE requests_total counter") {
		t.Error("missing TYPE comment for requests_total")
	}
	if !strings.Contains(body, `cpu_usage{id="svc_1",type="service"} 42.5`) {
		t.Errorf("missing or wrong cpu_usage metric line, got:\n%s", body)
	}
	if !strings.Contains(body, `requests_total{id="svc_1",type="service"} 100`) {
		t.Errorf("missing or wrong requests_total metric line, got:\n%s", body)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain content type, got %s", ct)
	}
}

func TestExporterLabelEscaping(t *testing.T) {
	points := []metrics.DataPoint{
		{
			Name:   "test_metric",
			Type:   metrics.Gauge,
			Labels: metrics.Labels{{Name: "path", Value: `value with "quotes"`}},
			Value:  1,
		},
		{
			Name:   "test_metric2",
			Type:   metrics.Gauge,
			Labels: metrics.Labels{{Name: "msg", Value: "line1\nline2"}},
			Value:  2,
		},
		{
			Name:   "test_metric3",
			Type:   metrics.Gauge,
			Labels: metrics.Labels{{Name: "path", Value: `back\slash`}},
			Value:  3,
		},
	}

	scraper := newTestScraper(points)
	exporter := NewExporter(scraper)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	exporter.ServeHTTP(w, req)

	body := w.Body.String()

	if !strings.Contains(body, `path="value with \"quotes\""`) {
		t.Errorf("quotes not escaped properly, got:\n%s", body)
	}
	if !strings.Contains(body, `msg="line1\nline2"`) {
		t.Errorf("newlines not escaped properly, got:\n%s", body)
	}
	if !strings.Contains(body, `path="back\\slash"`) {
		t.Errorf("backslashes not escaped properly, got:\n%s", body)
	}
}

func TestExporterSpecialValues(t *testing.T) {
	points := []metrics.DataPoint{
		{Name: "nan_metric", Type: metrics.Gauge, Value: math.NaN()},
		{Name: "pos_inf_metric", Type: metrics.Gauge, Value: math.Inf(1)},
		{Name: "neg_inf_metric", Type: metrics.Gauge, Value: math.Inf(-1)},
	}

	scraper := newTestScraper(points)
	exporter := NewExporter(scraper)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	exporter.ServeHTTP(w, req)

	body := w.Body.String()

	if !strings.Contains(body, "nan_metric NaN") {
		t.Errorf("expected NaN in output, got:\n%s", body)
	}
	if !strings.Contains(body, "pos_inf_metric +Inf") {
		t.Errorf("expected +Inf in output, got:\n%s", body)
	}
	if !strings.Contains(body, "neg_inf_metric -Inf") {
		t.Errorf("expected -Inf in output, got:\n%s", body)
	}
}

func TestExporterEmptyMetrics(t *testing.T) {
	scraper := newTestScraper(nil)
	exporter := NewExporter(scraper)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	exporter.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if body != "" {
		t.Errorf("expected empty output, got: %q", body)
	}
}

func TestExporterMultipleSeries(t *testing.T) {
	now := time.Now().UnixMilli()
	points := []metrics.DataPoint{
		{
			Name:      "http_requests",
			Type:      metrics.Counter,
			Labels:    metrics.Labels{{Name: "method", Value: "GET"}},
			Timestamp: now,
			Value:     100,
		},
		{
			Name:      "http_requests",
			Type:      metrics.Counter,
			Labels:    metrics.Labels{{Name: "method", Value: "POST"}},
			Timestamp: now,
			Value:     50,
		},
		{
			Name:      "http_requests",
			Type:      metrics.Counter,
			Labels:    metrics.Labels{{Name: "method", Value: "DELETE"}},
			Timestamp: now,
			Value:     10,
		},
	}

	scraper := newTestScraper(points)
	exporter := NewExporter(scraper)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	exporter.ServeHTTP(w, req)

	body := w.Body.String()

	// Should have exactly one TYPE declaration for http_requests.
	typeCount := strings.Count(body, "# TYPE http_requests counter")
	if typeCount != 1 {
		t.Errorf("expected 1 TYPE comment for http_requests, got %d\n%s", typeCount, body)
	}

	// All three series should appear.
	if !strings.Contains(body, `http_requests{method="GET"} 100`) {
		t.Errorf("missing GET series, got:\n%s", body)
	}
	if !strings.Contains(body, `http_requests{method="POST"} 50`) {
		t.Errorf("missing POST series, got:\n%s", body)
	}
	if !strings.Contains(body, `http_requests{method="DELETE"} 10`) {
		t.Errorf("missing DELETE series, got:\n%s", body)
	}
}
