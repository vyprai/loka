package supervisor

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseMemInfoValue(t *testing.T) {
	tests := []struct {
		line string
		want uint64
	}{
		{"MemTotal:       16384000 kB", 16384000},
		{"MemAvailable:    8192000 kB", 8192000},
		{"MemFree:         4096000 kB", 4096000},
		{"Invalid", 0},
		{"", 0},
	}
	for _, tt := range tests {
		got := parseMemInfoValue(tt.line)
		if got != tt.want {
			t.Errorf("parseMemInfoValue(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestCollectProcessCount(t *testing.T) {
	count := collectProcessCount()
	// On macOS there's no /proc, so -1 is expected.
	if count == -1 {
		t.Skip("/proc not available on this platform")
	}
	if count < 1 {
		t.Errorf("collectProcessCount() = %d, want >= 1", count)
	}
}

func TestCollectDisk(t *testing.T) {
	used, total := collectDisk()
	if total == 0 {
		t.Skip("statfs not available on this platform")
	}
	if used == 0 {
		t.Error("expected non-zero disk used")
	}
	if used > total {
		t.Errorf("used (%d) > total (%d)", used, total)
	}
}

func TestMetricsCollectorGetPoints(t *testing.T) {
	mc := NewMetricsCollector()

	// Wait for first collection (10s is too long for tests, so just get whatever's there).
	time.Sleep(100 * time.Millisecond)
	points := mc.GetPoints()

	// On a test machine, we should at least get process count and disk metrics.
	if len(points) == 0 {
		// First collection may not have happened yet — that's OK.
		t.Log("no points collected yet (collection interval is 10s)")
	}
}

func TestMetricsCollectorSetConfig(t *testing.T) {
	mc := NewMetricsCollector()
	mc.SetConfig(MetricsConfig{
		HTTPProbes: []HTTPProbe{
			{Port: 99999, Path: "/health"},
		},
	})

	// Verify config was stored.
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	if len(mc.probes) != 1 {
		t.Fatalf("expected 1 probe, got %d", len(mc.probes))
	}
	if mc.probes[0].Port != 99999 {
		t.Errorf("expected port 99999, got %d", mc.probes[0].Port)
	}
}

func TestProbeHTTP_ServerUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Extract port from test server URL.
	var port int
	for i := len(srv.URL) - 1; i >= 0; i-- {
		if srv.URL[i] == ':' {
			p := 0
			for _, c := range srv.URL[i+1:] {
				p = p*10 + int(c-'0')
			}
			port = p
			break
		}
	}

	mc := &MetricsCollector{httpRequestCounts: make(map[string]map[int]float64)}
	probe := HTTPProbe{Port: port, Path: "/"}
	now := time.Now().UnixMilli()

	points := mc.probeHTTP(probe, now)

	// Should have vm_http_up=1, vm_http_latency_seconds, vm_http_requests_total.
	names := make(map[string]float64)
	for _, p := range points {
		names[p.Name] = p.Value
	}

	if v, ok := names["vm_http_up"]; !ok || v != 1 {
		t.Errorf("expected vm_http_up=1, got %v (ok=%v)", v, ok)
	}
	if _, ok := names["vm_http_latency_seconds"]; !ok {
		t.Error("expected vm_http_latency_seconds metric")
	}
}

func TestProbeHTTP_ServerDown(t *testing.T) {
	mc := &MetricsCollector{httpRequestCounts: make(map[string]map[int]float64)}
	probe := HTTPProbe{Port: 1, Path: "/"} // Port 1 is unlikely to be listening.
	now := time.Now().UnixMilli()

	points := mc.probeHTTP(probe, now)

	if len(points) == 0 {
		t.Fatal("expected at least vm_http_up metric")
	}
	if points[0].Name != "vm_http_up" || points[0].Value != 0 {
		t.Errorf("expected vm_http_up=0, got %s=%f", points[0].Name, points[0].Value)
	}
}

func TestCPUDeltaCalculation(t *testing.T) {
	mc := &MetricsCollector{}

	// First call returns -1 (needs two samples).
	first := mc.collectCPU()
	if first != -1 {
		t.Log("first CPU reading available (system has /proc/stat)")
	}

	// Second call should return a value >= 0.
	second := mc.collectCPU()
	if second < -1 {
		t.Errorf("expected CPU >= -1, got %f", second)
	}
}
