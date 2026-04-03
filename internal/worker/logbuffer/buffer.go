// Package logbuffer provides an in-memory log buffer for the worker.
// It receives log lines pushed from supervisors via vsock and exposes
// a GET /logs endpoint for the CP to scrape.
package logbuffer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Entry is a buffered log entry (matches supervisor LogLine format).
type Entry struct {
	Timestamp time.Time         `json:"ts"`
	Level     string            `json:"level"`
	Message   string            `json:"msg"`
	Labels    map[string]string `json:"labels"`
}

// Buffer is a circular buffer of log entries with HTTP exporter.
type Buffer struct {
	mu      sync.RWMutex
	entries []Entry
	cap     int
	pos     int
	full    bool
}

// New creates a log buffer with the given capacity.
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 50000
	}
	return &Buffer{
		entries: make([]Entry, capacity),
		cap:     capacity,
	}
}

// Add appends a log entry to the buffer.
func (b *Buffer) Add(entry Entry) {
	b.mu.Lock()
	b.entries[b.pos] = entry
	b.pos = (b.pos + 1) % b.cap
	if b.pos == 0 {
		b.full = true
	}
	b.mu.Unlock()
}

// AddFromJSON parses a JSON-encoded log line from the supervisor and adds it to the buffer.
func (b *Buffer) AddFromJSON(data []byte, workerID string) error {
	var line struct {
		Timestamp time.Time `json:"ts"`
		Level     string    `json:"level"`
		Message   string    `json:"msg"`
		Type      string    `json:"type"`
		ID        string    `json:"id"`
		Stream    string    `json:"stream"`
	}
	if err := json.Unmarshal(data, &line); err != nil {
		return err
	}

	entry := Entry{
		Timestamp: line.Timestamp,
		Level:     line.Level,
		Message:   line.Message,
		Labels: map[string]string{
			"source":    "supervisor",
			"type":      line.Type,
			"id":        line.ID,
			"stream":    line.Stream,
			"worker_id": workerID,
			"level":     line.Level,
		},
	}
	b.Add(entry)
	return nil
}

// Query returns entries within the time range, up to the limit.
func (b *Buffer) Query(start, end time.Time, limit int) []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var count int
	if b.full {
		count = b.cap
	} else {
		count = b.pos
	}
	if limit <= 0 {
		limit = 1000
	}

	var result []Entry
	// Read in chronological order.
	startIdx := 0
	if b.full {
		startIdx = b.pos
	}

	for i := 0; i < count && len(result) < limit; i++ {
		idx := (startIdx + i) % b.cap
		e := b.entries[idx]
		if !e.Timestamp.IsZero() && !e.Timestamp.Before(start) && !e.Timestamp.After(end) {
			result = append(result, e)
		}
	}
	return result
}

// Count returns the number of buffered entries.
func (b *Buffer) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.full {
		return b.cap
	}
	return b.pos
}

// ServeHTTP handles GET /logs requests.
// Query params: start, end (RFC3339), limit (int).
func (b *Buffer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now().Add(-time.Hour)
	end := time.Now()
	limit := 1000

	if s := r.URL.Query().Get("start"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			start = t
		}
	}
	if s := r.URL.Query().Get("end"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			end = t
		}
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		fmt.Sscanf(s, "%d", &limit)
	}

	entries := b.Query(start, end, limit)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
	})
}
