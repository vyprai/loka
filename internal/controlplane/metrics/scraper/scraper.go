package scraper

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
)

// ScrapeTarget represents a target to scrape metrics from.
type ScrapeTarget struct {
	Address string            // e.g. "10.0.1.5:6850"
	Type    string            // "worker" or "gateway"
	Labels  map[string]string // added to all scraped metrics
}

// TargetDiscovery discovers scrape targets.
type TargetDiscovery interface {
	Targets(ctx context.Context) ([]ScrapeTarget, error)
}

// Scraper periodically scrapes /metrics endpoints from discovered targets.
type Scraper struct {
	store      tsdb.MetricsStore
	discovery  TargetDiscovery
	interval   time.Duration
	httpClient *http.Client
	logger     *slog.Logger
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	mu          sync.RWMutex
	lastScrapes map[string]*ScrapeResult // keyed by address
}

// ScrapeResult stores the result of the last scrape for a target.
type ScrapeResult struct {
	Target         ScrapeTarget
	Up             bool
	LastScrape     time.Time
	Duration       time.Duration
	SamplesScraped int
	Error          string
}

// New creates a new Scraper.
func New(store tsdb.MetricsStore, discovery TargetDiscovery, interval time.Duration, logger *slog.Logger) *Scraper {
	if interval == 0 {
		interval = 15 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scraper{
		store:     store,
		discovery: discovery,
		interval:  interval,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger:      logger,
		lastScrapes: make(map[string]*ScrapeResult),
	}
}

// Start begins the scrape loop.
func (s *Scraper) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop stops the scrape loop.
func (s *Scraper) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// Targets returns the last scrape results for all targets.
func (s *Scraper) Targets() []*ScrapeResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	results := make([]*ScrapeResult, 0, len(s.lastScrapes))
	for _, r := range s.lastScrapes {
		results = append(results, r)
	}
	return results
}

func (s *Scraper) run(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// Scrape immediately on start.
	s.scrapeAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scrapeAll(ctx)
		}
	}
}

func (s *Scraper) scrapeAll(ctx context.Context) {
	targets, err := s.discovery.Targets(ctx)
	if err != nil {
		s.logger.Warn("scraper: failed to discover targets", "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(t ScrapeTarget) {
			defer wg.Done()
			s.scrapeTarget(ctx, t)
		}(target)
	}
	wg.Wait()
}

func (s *Scraper) scrapeTarget(ctx context.Context, target ScrapeTarget) {
	start := time.Now()
	result := &ScrapeResult{
		Target:     target,
		LastScrape: start,
	}

	url := fmt.Sprintf("http://%s/metrics", target.Address)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		result.Error = err.Error()
		s.storeResult(target.Address, result)
		return
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		result.Error = err.Error()
		s.storeResult(target.Address, result)
		s.writeScrapeMetrics(ctx, target, result)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		s.storeResult(target.Address, result)
		s.writeScrapeMetrics(ctx, target, result)
		return
	}

	// Parse Prometheus text format.
	now := time.Now().UnixMilli()
	points, err := parsePrometheusText(resp.Body, target.Labels, now)
	if err != nil {
		result.Error = err.Error()
		s.storeResult(target.Address, result)
		s.writeScrapeMetrics(ctx, target, result)
		return
	}

	result.Up = true
	result.Duration = time.Since(start)
	result.SamplesScraped = len(points)

	if len(points) > 0 {
		if err := s.store.Write(ctx, points); err != nil {
			s.logger.Warn("scraper: failed to write scraped metrics", "target", target.Address, "error", err)
		}
	}

	s.storeResult(target.Address, result)
	s.writeScrapeMetrics(ctx, target, result)
}

func (s *Scraper) storeResult(address string, result *ScrapeResult) {
	s.mu.Lock()
	s.lastScrapes[address] = result
	s.mu.Unlock()
}

func (s *Scraper) writeScrapeMetrics(ctx context.Context, target ScrapeTarget, result *ScrapeResult) {
	now := time.Now().UnixMilli()
	upVal := 0.0
	if result.Up {
		upVal = 1.0
	}

	points := []metrics.DataPoint{
		{
			Name:      "scrape_up",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "target", Value: target.Address}, {Name: "target_type", Value: target.Type}},
			Timestamp: now,
			Value:     upVal,
		},
		{
			Name:      "scrape_duration_seconds",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "target", Value: target.Address}, {Name: "target_type", Value: target.Type}},
			Timestamp: now,
			Value:     result.Duration.Seconds(),
		},
		{
			Name:      "scrape_samples_scraped",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "target", Value: target.Address}, {Name: "target_type", Value: target.Type}},
			Timestamp: now,
			Value:     float64(result.SamplesScraped),
		},
	}

	if err := s.store.Write(ctx, points); err != nil {
		s.logger.Warn("scraper: failed to write scrape metrics", "error", err)
	}
}

// parsePrometheusText parses the Prometheus text exposition format.
// It handles HELP, TYPE comments, and metric lines.
func parsePrometheusText(r interface{ Read([]byte) (int, error) }, extraLabels map[string]string, timestampMs int64) ([]metrics.DataPoint, error) {
	scanner := bufio.NewScanner(r)
	var points []metrics.DataPoint
	typeMap := make(map[string]metrics.MetricType)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "# HELP") {
			continue
		}
		if strings.HasPrefix(line, "# TYPE") {
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				name := parts[2]
				switch parts[3] {
				case "counter":
					typeMap[name] = metrics.Counter
				case "gauge":
					typeMap[name] = metrics.Gauge
				case "histogram":
					typeMap[name] = metrics.Histogram
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		dp, err := parseMetricLine(line, typeMap, extraLabels, timestampMs)
		if err != nil {
			continue // skip unparseable lines
		}
		points = append(points, dp)
	}

	return points, scanner.Err()
}

// parseMetricLine parses a single Prometheus metric line like:
// metric_name{label1="value1",label2="value2"} 42.5 1234567890
func parseMetricLine(line string, typeMap map[string]metrics.MetricType, extraLabels map[string]string, defaultTs int64) (metrics.DataPoint, error) {
	var dp metrics.DataPoint

	// Find the value portion (after labels or metric name).
	nameEnd := strings.IndexByte(line, '{')
	var labelsStr string
	var rest string

	if nameEnd >= 0 {
		dp.Name = line[:nameEnd]
		closeBrace := strings.IndexByte(line[nameEnd:], '}')
		if closeBrace < 0 {
			return dp, fmt.Errorf("unclosed brace")
		}
		labelsStr = line[nameEnd+1 : nameEnd+closeBrace]
		rest = strings.TrimSpace(line[nameEnd+closeBrace+1:])
	} else {
		// No labels.
		spaceIdx := strings.IndexByte(line, ' ')
		if spaceIdx < 0 {
			return dp, fmt.Errorf("no value")
		}
		dp.Name = line[:spaceIdx]
		rest = strings.TrimSpace(line[spaceIdx+1:])
	}

	// Parse value (and optional timestamp).
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return dp, fmt.Errorf("no value")
	}

	val, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return dp, fmt.Errorf("invalid value: %w", err)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		// Store NaN/Inf as-is for histogram buckets.
	}
	dp.Value = val

	dp.Timestamp = defaultTs
	if len(fields) >= 2 {
		if ts, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			dp.Timestamp = ts
		}
	}

	// Parse labels.
	if labelsStr != "" {
		dp.Labels = parseLabels(labelsStr)
	}

	// Add extra labels from target.
	for k, v := range extraLabels {
		dp.Labels = append(dp.Labels, metrics.Label{Name: k, Value: v})
	}
	dp.Labels.Sort()

	// Determine type from name suffix or type map.
	baseName := dp.Name
	if strings.HasSuffix(baseName, "_total") {
		baseName = strings.TrimSuffix(baseName, "_total")
	} else if strings.HasSuffix(baseName, "_bucket") {
		baseName = strings.TrimSuffix(baseName, "_bucket")
	} else if strings.HasSuffix(baseName, "_sum") {
		baseName = strings.TrimSuffix(baseName, "_sum")
	} else if strings.HasSuffix(baseName, "_count") {
		baseName = strings.TrimSuffix(baseName, "_count")
	}

	if mt, ok := typeMap[baseName]; ok {
		dp.Type = mt
	} else if mt, ok := typeMap[dp.Name]; ok {
		dp.Type = mt
	} else if strings.HasSuffix(dp.Name, "_total") {
		dp.Type = metrics.Counter
	}

	return dp, nil
}

// parseLabels parses a Prometheus label string like: label1="value1",label2="value2"
func parseLabels(s string) metrics.Labels {
	var labels metrics.Labels
	// Simple state machine parser for label pairs.
	for s != "" {
		s = strings.TrimSpace(s)
		if s == "" {
			break
		}

		eqIdx := strings.IndexByte(s, '=')
		if eqIdx < 0 {
			break
		}
		name := strings.TrimSpace(s[:eqIdx])
		s = s[eqIdx+1:]

		if len(s) == 0 || s[0] != '"' {
			break
		}
		s = s[1:] // skip opening quote

		// Find closing quote (handle escaped quotes).
		var value strings.Builder
		escaped := false
		i := 0
		for i < len(s) {
			if escaped {
				switch s[i] {
				case 'n':
					value.WriteByte('\n')
				case '\\':
					value.WriteByte('\\')
				case '"':
					value.WriteByte('"')
				default:
					value.WriteByte('\\')
					value.WriteByte(s[i])
				}
				escaped = false
			} else if s[i] == '\\' {
				escaped = true
			} else if s[i] == '"' {
				break
			} else {
				value.WriteByte(s[i])
			}
			i++
		}

		labels = append(labels, metrics.Label{Name: name, Value: value.String()})

		if i < len(s) {
			s = s[i+1:] // skip closing quote
		} else {
			break
		}

		// Skip comma separator.
		s = strings.TrimLeft(s, ", ")
	}

	return labels
}
