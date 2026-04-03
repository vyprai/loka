package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/logging"
	"github.com/vyprai/loka/internal/controlplane/logging/store"
)

// ScrapeTarget represents a target to scrape logs from.
type ScrapeTarget struct {
	Address string            // e.g. "10.0.1.5:6850"
	Type    string            // "worker"
	Labels  map[string]string // added to all scraped log entries
}

// TargetDiscovery discovers scrape targets.
type TargetDiscovery interface {
	Targets(ctx context.Context) ([]ScrapeTarget, error)
}

// LogScraper periodically scrapes /logs endpoints from discovered targets.
type LogScraper struct {
	store      store.LogStore
	discovery  TargetDiscovery
	interval   time.Duration
	httpClient *http.Client
	logger     *slog.Logger
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	mu         sync.RWMutex
	lastCursor map[string]time.Time // per-target, last scraped timestamp
}

// ScrapeResult stores the result of the last scrape for a target.
type ScrapeResult struct {
	Target       ScrapeTarget
	Up           bool
	LastScrape   time.Time
	Duration     time.Duration
	EntriesCount int
	Error        string
}

// New creates a new LogScraper.
func New(logStore store.LogStore, discovery TargetDiscovery, interval time.Duration, logger *slog.Logger) *LogScraper {
	if interval == 0 {
		interval = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LogScraper{
		store:     logStore,
		discovery: discovery,
		interval:  interval,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger:     logger,
		lastCursor: make(map[string]time.Time),
	}
}

// Start begins the scrape loop.
func (s *LogScraper) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop stops the scrape loop.
func (s *LogScraper) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *LogScraper) run(ctx context.Context) {
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

func (s *LogScraper) scrapeAll(ctx context.Context) {
	targets, err := s.discovery.Targets(ctx)
	if err != nil {
		s.logger.Warn("log-scraper: failed to discover targets", "error", err)
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

func (s *LogScraper) scrapeTarget(ctx context.Context, target ScrapeTarget) {
	s.mu.RLock()
	cursor := s.lastCursor[target.Address]
	s.mu.RUnlock()

	if cursor.IsZero() {
		cursor = time.Now().Add(-5 * time.Minute)
	}

	url := fmt.Sprintf("http://%s/logs?start=%s&limit=5000",
		target.Address,
		cursor.Format(time.RFC3339Nano),
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		s.logger.Warn("log-scraper: request creation failed", "target", target.Address, "error", err)
		return
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Warn("log-scraper: scrape failed", "target", target.Address, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("log-scraper: bad status", "target", target.Address, "status", resp.StatusCode)
		return
	}

	// Parse the response from the worker's logbuffer.
	var body struct {
		Entries []workerEntry `json:"entries"`
		Count   int           `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		s.logger.Warn("log-scraper: decode failed", "target", target.Address, "error", err)
		return
	}

	if len(body.Entries) == 0 {
		return
	}

	// Convert to LogEntry with proper labels.
	entries := make([]logging.LogEntry, 0, len(body.Entries))
	var maxTS time.Time

	for _, we := range body.Entries {
		labels := make(map[string]string, len(we.Labels)+len(target.Labels))
		for k, v := range we.Labels {
			labels[k] = v
		}
		for k, v := range target.Labels {
			labels[k] = v
		}

		entry := logging.LogEntry{
			Timestamp: we.Timestamp,
			Level:     we.Level,
			Message:   we.Message,
			Labels:    labels,
		}
		entries = append(entries, entry)

		if we.Timestamp.After(maxTS) {
			maxTS = we.Timestamp
		}
	}

	// Write batch to store.
	if err := s.store.Write(ctx, entries); err != nil {
		s.logger.Warn("log-scraper: write failed", "target", target.Address, "error", err)
		return
	}

	// Update cursor to just after the latest entry to avoid duplicates.
	s.mu.Lock()
	s.lastCursor[target.Address] = maxTS.Add(time.Nanosecond)
	s.mu.Unlock()

	s.logger.Debug("log-scraper: scraped entries", "target", target.Address, "count", len(entries))
}

// workerEntry matches the Entry struct from the worker's logbuffer package.
type workerEntry struct {
	Timestamp time.Time         `json:"ts"`
	Level     string            `json:"level"`
	Message   string            `json:"msg"`
	Labels    map[string]string `json:"labels"`
}
