package gateway

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vyprai/loka/internal/metrics"
)

// serviceStats holds per-service counters tracked by the MetricsTracker.
type serviceStats struct {
	ServiceID   string
	ServiceName string
	Domain      string

	TotalRequests     atomic.Int64
	ActiveConnections atomic.Int64
	BytesSent         atomic.Int64
	BytesReceived     atomic.Int64

	// latestDuration stores the most recent request duration in nanoseconds.
	latestDuration atomic.Int64

	// statusCounts tracks request counts per HTTP status code.
	mu           sync.Mutex
	statusCounts map[int]*atomic.Int64
}

func newServiceStats(serviceID, serviceName, domain string) *serviceStats {
	return &serviceStats{
		ServiceID:    serviceID,
		ServiceName:  serviceName,
		Domain:       domain,
		statusCounts: make(map[int]*atomic.Int64),
	}
}

func (s *serviceStats) incrStatus(code int) {
	s.mu.Lock()
	counter, ok := s.statusCounts[code]
	if !ok {
		counter = &atomic.Int64{}
		s.statusCounts[code] = counter
	}
	s.mu.Unlock()
	counter.Add(1)
}

func (s *serviceStats) snapshotStatusCounts() map[int]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]int64, len(s.statusCounts))
	for code, counter := range s.statusCounts {
		out[code] = counter.Load()
	}
	return out
}

// MetricsTracker collects per-service gateway metrics and exposes them
// via a Prometheus-compatible /metrics endpoint.
type MetricsTracker struct {
	mu       sync.RWMutex
	services map[string]*serviceStats // serviceID → stats
}

// NewMetricsTracker creates a new MetricsTracker.
func NewMetricsTracker() *MetricsTracker {
	return &MetricsTracker{
		services: make(map[string]*serviceStats),
	}
}

// RecordRequest records a completed request for the given service.
func (mt *MetricsTracker) RecordRequest(serviceID, serviceName, domain string, statusCode int, duration time.Duration, bytesSent, bytesReceived int64) {
	s := mt.getOrCreate(serviceID, serviceName, domain)

	s.TotalRequests.Add(1)
	s.latestDuration.Store(int64(duration))
	s.BytesSent.Add(bytesSent)
	s.BytesReceived.Add(bytesReceived)
	s.incrStatus(statusCode)
}

// SetActiveConnections sets the active connection gauge for a service.
func (mt *MetricsTracker) SetActiveConnections(serviceID, serviceName, domain string, n int64) {
	s := mt.getOrCreate(serviceID, serviceName, domain)
	s.ActiveConnections.Store(n)
}

func (mt *MetricsTracker) getOrCreate(serviceID, serviceName, domain string) *serviceStats {
	mt.mu.RLock()
	s, ok := mt.services[serviceID]
	mt.mu.RUnlock()
	if ok {
		return s
	}

	mt.mu.Lock()
	defer mt.mu.Unlock()
	// Double-check after acquiring write lock.
	if s, ok = mt.services[serviceID]; ok {
		return s
	}
	s = newServiceStats(serviceID, serviceName, domain)
	mt.services[serviceID] = s
	return s
}

// ServeHTTP handles GET /metrics in Prometheus text exposition format.
func (mt *MetricsTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	points := mt.collect()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Group by metric name for TYPE comments.
	grouped := make(map[string][]metrics.DataPoint)
	var names []string
	for _, p := range points {
		if _, ok := grouped[p.Name]; !ok {
			names = append(names, p.Name)
		}
		grouped[p.Name] = append(grouped[p.Name], p)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		pts := grouped[name]
		if len(pts) == 0 {
			continue
		}
		typeStr := "gauge"
		switch pts[0].Type {
		case metrics.Counter:
			typeStr = "counter"
		case metrics.Histogram:
			typeStr = "histogram"
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, typeStr)

		for _, p := range pts {
			writePoint(&b, p)
		}
	}

	w.Write([]byte(b.String()))
}

func (mt *MetricsTracker) collect() []metrics.DataPoint {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	var points []metrics.DataPoint

	for _, s := range mt.services {
		id := "service_" + s.ServiceID
		baseLabels := metrics.Labels{
			{Name: "domain", Value: s.Domain},
			{Name: "id", Value: id},
			{Name: "name", Value: s.ServiceName},
			{Name: "type", Value: "service"},
		}

		// gateway_requests_total — one series per status code.
		for code, count := range s.snapshotStatusCounts() {
			lbls := make(metrics.Labels, len(baseLabels)+1)
			copy(lbls, baseLabels)
			lbls[len(baseLabels)] = metrics.Label{Name: "status_code", Value: fmt.Sprintf("%d", code)}
			lbls.Sort()
			points = append(points, metrics.DataPoint{
				Name:   "gateway_requests_total",
				Type:   metrics.Counter,
				Labels: lbls,
				Value:  float64(count),
			})
		}

		// gateway_request_duration_seconds
		dur := time.Duration(s.latestDuration.Load())
		points = append(points, metrics.DataPoint{
			Name:   "gateway_request_duration_seconds",
			Type:   metrics.Gauge,
			Labels: copyLabels(baseLabels),
			Value:  dur.Seconds(),
		})

		// gateway_active_connections
		points = append(points, metrics.DataPoint{
			Name:   "gateway_active_connections",
			Type:   metrics.Gauge,
			Labels: copyLabels(baseLabels),
			Value:  float64(s.ActiveConnections.Load()),
		})

		// gateway_bytes_sent_total
		points = append(points, metrics.DataPoint{
			Name:   "gateway_bytes_sent_total",
			Type:   metrics.Counter,
			Labels: copyLabels(baseLabels),
			Value:  float64(s.BytesSent.Load()),
		})

		// gateway_bytes_received_total
		points = append(points, metrics.DataPoint{
			Name:   "gateway_bytes_received_total",
			Type:   metrics.Counter,
			Labels: copyLabels(baseLabels),
			Value:  float64(s.BytesReceived.Load()),
		})
	}

	return points
}

func copyLabels(src metrics.Labels) metrics.Labels {
	dst := make(metrics.Labels, len(src))
	copy(dst, src)
	return dst
}

func writePoint(b *strings.Builder, p metrics.DataPoint) {
	b.WriteString(p.Name)
	if len(p.Labels) > 0 {
		b.WriteByte('{')
		for i, l := range p.Labels {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, "%s=\"%s\"", l.Name, escapeLabel(l.Value))
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	if math.IsNaN(p.Value) {
		b.WriteString("NaN")
	} else if math.IsInf(p.Value, 1) {
		b.WriteString("+Inf")
	} else if math.IsInf(p.Value, -1) {
		b.WriteString("-Inf")
	} else {
		fmt.Fprintf(b, "%g", p.Value)
	}
	b.WriteByte('\n')
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}
