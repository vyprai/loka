package workermetrics

import (
	"encoding/json"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/metrics"
	"github.com/vyprai/loka/internal/worker/vm"
)

// SessionInfo holds the identity info for a VM session.
type SessionInfo struct {
	ID        string // session/service/task ID
	Name      string
	Type      string // "session", "service", "task"
	ImageRef  string
	WorkerID  string
	Vsock     *vm.VsockClient
}

// Scraper periodically scrapes VM metrics via vsock and collects host metrics.
type Scraper struct {
	mu       sync.RWMutex
	sessions map[string]*SessionInfo // keyed by session ID
	points   []metrics.DataPoint     // latest snapshot

	workerID       string
	workerHostname string
	provider       string
	region         string
	interval       time.Duration
	logger         *slog.Logger
	cancel         chan struct{}
}

// NewScraper creates a new worker-side metrics scraper.
func NewScraper(workerID, hostname, provider, region string, interval time.Duration, logger *slog.Logger) *Scraper {
	if interval == 0 {
		interval = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Scraper{
		sessions:       make(map[string]*SessionInfo),
		workerID:       workerID,
		workerHostname: hostname,
		provider:       provider,
		region:         region,
		interval:       interval,
		logger:         logger,
		cancel:         make(chan struct{}),
	}
	go s.collectLoop()
	return s
}

// AddSession registers a VM session for metrics scraping.
func (s *Scraper) AddSession(info *SessionInfo) {
	s.mu.Lock()
	s.sessions[info.ID] = info
	s.mu.Unlock()
}

// RemoveSession unregisters a VM session.
func (s *Scraper) RemoveSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// GetPoints returns the latest metrics snapshot.
func (s *Scraper) GetPoints() []metrics.DataPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]metrics.DataPoint, len(s.points))
	copy(result, s.points)
	return result
}

// Stop stops the scraper.
func (s *Scraper) Stop() {
	close(s.cancel)
}

func (s *Scraper) collectLoop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.collect()

	for {
		select {
		case <-s.cancel:
			return
		case <-ticker.C:
			s.collect()
		}
	}
}

func (s *Scraper) collect() {
	now := time.Now().UnixMilli()
	var points []metrics.DataPoint

	// Host-level metrics.
	points = append(points, s.collectHost(now)...)

	// Per-session VM metrics via vsock.
	s.mu.RLock()
	sessions := make([]*SessionInfo, 0, len(s.sessions))
	for _, info := range s.sessions {
		sessions = append(sessions, info)
	}
	s.mu.RUnlock()

	for _, info := range sessions {
		vmPoints := s.scrapeSession(info)
		points = append(points, vmPoints...)
	}

	s.mu.Lock()
	s.points = points
	s.mu.Unlock()
}

func (s *Scraper) collectHost(now int64) []metrics.DataPoint {
	workerLabels := metrics.Labels{
		{Name: "id", Value: "worker_" + s.workerID},
		{Name: "type", Value: "worker"},
		{Name: "name", Value: s.workerHostname},
		{Name: "provider", Value: s.provider},
		{Name: "region", Value: s.region},
	}
	workerLabels.Sort()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	s.mu.RLock()
	sessionCount := len(s.sessions)
	s.mu.RUnlock()

	return []metrics.DataPoint{
		{Name: "worker_memory_used_bytes", Type: metrics.Gauge, Labels: workerLabels, Timestamp: now, Value: float64(m.Sys)},
		{Name: "worker_active_sessions", Type: metrics.Gauge, Labels: workerLabels, Timestamp: now, Value: float64(sessionCount)},
		{Name: "worker_active_vms", Type: metrics.Gauge, Labels: workerLabels, Timestamp: now, Value: float64(sessionCount)},
	}
}

func (s *Scraper) scrapeSession(info *SessionInfo) []metrics.DataPoint {
	if info.Vsock == nil {
		return nil
	}

	// Call metrics_scrape RPC via vsock.
	resp, err := info.Vsock.MetricsScrape()
	if err != nil {
		s.logger.Debug("failed to scrape VM metrics", "session", info.ID, "error", err)
		return nil
	}

	var result struct {
		Points []metrics.DataPoint `json:"points"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		s.logger.Debug("failed to parse VM metrics", "session", info.ID, "error", err)
		return nil
	}

	// Enrich with identity labels.
	idPrefix := info.Type + "_"
	for i := range result.Points {
		result.Points[i].Labels = append(result.Points[i].Labels,
			metrics.Label{Name: "id", Value: idPrefix + info.ID},
			metrics.Label{Name: "type", Value: info.Type},
			metrics.Label{Name: "name", Value: info.Name},
			metrics.Label{Name: "worker_id", Value: s.workerID},
			metrics.Label{Name: "image_ref", Value: info.ImageRef},
		)
		result.Points[i].Labels.Sort()
	}

	return result.Points
}
