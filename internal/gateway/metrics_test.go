package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordRequest(t *testing.T) {
	mt := NewMetricsTracker()

	mt.RecordRequest("svc-1", "web", "example.com", 200, 50*time.Millisecond, 1024, 512)
	mt.RecordRequest("svc-1", "web", "example.com", 200, 30*time.Millisecond, 2048, 256)

	mt.mu.RLock()
	s := mt.services["svc-1"]
	mt.mu.RUnlock()

	if s == nil {
		t.Fatal("expected service svc-1 to exist")
	}
	if got := s.TotalRequests.Load(); got != 2 {
		t.Errorf("expected 2 total requests, got %d", got)
	}
	if got := s.BytesSent.Load(); got != 3072 {
		t.Errorf("expected 3072 bytes sent, got %d", got)
	}
	if got := s.BytesReceived.Load(); got != 768 {
		t.Errorf("expected 768 bytes received, got %d", got)
	}
}

func TestConcurrentRecordRequest(t *testing.T) {
	mt := NewMetricsTracker()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mt.RecordRequest("svc-1", "api", "api.example.com", 200, time.Millisecond, 100, 50)
		}()
	}
	wg.Wait()

	mt.mu.RLock()
	s := mt.services["svc-1"]
	mt.mu.RUnlock()

	if s == nil {
		t.Fatal("expected service svc-1 to exist")
	}
	if got := s.TotalRequests.Load(); got != 100 {
		t.Errorf("expected 100 total requests, got %d", got)
	}
	if got := s.BytesSent.Load(); got != 10000 {
		t.Errorf("expected 10000 bytes sent, got %d", got)
	}
}

func TestServeHTTPOutput(t *testing.T) {
	mt := NewMetricsTracker()

	mt.RecordRequest("svc-1", "web", "example.com", 200, 10*time.Millisecond, 100, 50)
	mt.RecordRequest("svc-1", "web", "example.com", 500, 20*time.Millisecond, 200, 100)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain content type, got %s", ct)
	}

	if !strings.Contains(body, "# TYPE gateway_requests_total counter") {
		t.Errorf("missing TYPE comment for gateway_requests_total, got:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE gateway_active_connections gauge") {
		t.Errorf("missing TYPE comment for gateway_active_connections, got:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE gateway_bytes_sent_total counter") {
		t.Errorf("missing TYPE comment for gateway_bytes_sent_total, got:\n%s", body)
	}
	if !strings.Contains(body, "gateway_requests_total") {
		t.Errorf("missing gateway_requests_total metric, got:\n%s", body)
	}
}

func TestSetActiveConnections(t *testing.T) {
	mt := NewMetricsTracker()

	mt.SetActiveConnections("svc-1", "web", "example.com", 42)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	body := w.Body.String()

	if !strings.Contains(body, "gateway_active_connections") {
		t.Errorf("missing gateway_active_connections metric, got:\n%s", body)
	}
	if !strings.Contains(body, "42") {
		t.Errorf("expected active connections value 42 in output, got:\n%s", body)
	}
}
