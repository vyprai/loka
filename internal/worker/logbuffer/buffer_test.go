package logbuffer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBufferAddQuery(t *testing.T) {
	buf := New(100)
	now := time.Now()

	buf.Add(Entry{Timestamp: now, Level: "info", Message: "hello", Labels: map[string]string{"type": "service"}})
	buf.Add(Entry{Timestamp: now.Add(time.Second), Level: "warn", Message: "world", Labels: map[string]string{"type": "task"}})

	results := buf.Query(now.Add(-time.Minute), now.Add(time.Minute), 10)
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}
	if results[0].Message != "hello" {
		t.Fatalf("expected 'hello', got %q", results[0].Message)
	}
	if results[1].Message != "world" {
		t.Fatalf("expected 'world', got %q", results[1].Message)
	}
}

func TestBufferAddFromJSON(t *testing.T) {
	buf := New(100)
	now := time.Now()

	data, _ := json.Marshal(map[string]interface{}{
		"ts":     now,
		"level":  "error",
		"msg":    "disk full",
		"type":   "system",
		"id":     "sys-1",
		"stream": "stderr",
	})

	if err := buf.AddFromJSON(data, "worker-42"); err != nil {
		t.Fatal(err)
	}

	results := buf.Query(now.Add(-time.Minute), now.Add(time.Minute), 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}

	e := results[0]
	if e.Message != "disk full" {
		t.Fatalf("expected 'disk full', got %q", e.Message)
	}
	if e.Labels["worker_id"] != "worker-42" {
		t.Fatalf("expected worker_id=worker-42, got %s", e.Labels["worker_id"])
	}
	if e.Labels["type"] != "system" {
		t.Fatalf("expected type=system, got %s", e.Labels["type"])
	}
	if e.Labels["stream"] != "stderr" {
		t.Fatalf("expected stream=stderr, got %s", e.Labels["stream"])
	}
}

func TestBufferCircularWrap(t *testing.T) {
	cap := 5
	buf := New(cap)
	now := time.Now()

	// Add more entries than capacity.
	for i := 0; i < 8; i++ {
		buf.Add(Entry{
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Level:     "info",
			Message:   string(rune('A' + i)),
		})
	}

	if buf.Count() != cap {
		t.Fatalf("expected count=%d, got %d", cap, buf.Count())
	}

	results := buf.Query(now.Add(-time.Minute), now.Add(time.Hour), 100)
	if len(results) != cap {
		t.Fatalf("expected %d entries, got %d", cap, len(results))
	}

	// Oldest entries (A, B, C) should be dropped; we should have D, E, F, G, H.
	// Entries 3..7 correspond to messages D, E, F, G, H.
	for i, e := range results {
		expected := string(rune('A' + 3 + i))
		if e.Message != expected {
			t.Fatalf("entry %d: expected %q, got %q", i, expected, e.Message)
		}
	}
}

func TestBufferQueryTimeRange(t *testing.T) {
	buf := New(100)
	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	buf.Add(Entry{Timestamp: base, Level: "info", Message: "at-noon"})
	buf.Add(Entry{Timestamp: base.Add(30 * time.Minute), Level: "info", Message: "at-12:30"})
	buf.Add(Entry{Timestamp: base.Add(2 * time.Hour), Level: "info", Message: "at-2pm"})

	// Query only the first hour.
	results := buf.Query(base, base.Add(time.Hour), 100)
	if len(results) != 2 {
		t.Fatalf("expected 2 entries in range, got %d", len(results))
	}
	if results[0].Message != "at-noon" {
		t.Fatalf("expected 'at-noon', got %q", results[0].Message)
	}
	if results[1].Message != "at-12:30" {
		t.Fatalf("expected 'at-12:30', got %q", results[1].Message)
	}
}

func TestBufferServeHTTP(t *testing.T) {
	buf := New(100)
	now := time.Now()
	buf.Add(Entry{Timestamp: now, Level: "info", Message: "http test", Labels: map[string]string{"type": "service"}})

	req := httptest.NewRequest("GET", "/logs?start="+now.Add(-time.Hour).Format(time.RFC3339)+"&end="+now.Add(time.Hour).Format(time.RFC3339)+"&limit=10", nil)
	w := httptest.NewRecorder()
	buf.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Entries []Entry `json:"entries"`
		Count   int     `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected count=1, got %d", resp.Count)
	}
	if resp.Entries[0].Message != "http test" {
		t.Fatalf("expected 'http test', got %q", resp.Entries[0].Message)
	}
}

func TestBufferConcurrentAdd(t *testing.T) {
	buf := New(1000)
	now := time.Now()

	var wg sync.WaitGroup
	entriesPerGoroutine := 50
	goroutines := 10

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < entriesPerGoroutine; i++ {
				buf.Add(Entry{
					Timestamp: now.Add(time.Duration(id*entriesPerGoroutine+i) * time.Millisecond),
					Level:     "info",
					Message:   "concurrent",
				})
			}
		}(g)
	}
	wg.Wait()

	total := goroutines * entriesPerGoroutine
	if buf.Count() != total {
		t.Fatalf("expected %d entries, got %d", total, buf.Count())
	}
}
