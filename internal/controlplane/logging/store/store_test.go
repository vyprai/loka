package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/logging"
)

func newTestStore(t *testing.T) LogStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(StoreConfig{
		DataDir:   dir,
		Retention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWriteQueryRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entries := []logging.LogEntry{
		{
			Timestamp: now,
			Level:     "info",
			Message:   "hello world",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
		{
			Timestamp: now.Add(time.Second),
			Level:     "error",
			Message:   "something failed",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	result, err := s.Query(ctx, logging.QueryRequest{
		Query:     `{source="cp"}`,
		Start:     now.Add(-time.Minute),
		End:       now.Add(time.Minute),
		Limit:     100,
		Direction: "forward",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(result.Streams))
	}
	if len(result.Streams[0].Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result.Streams[0].Entries))
	}
	if result.Streams[0].Entries[0].Message != "hello world" {
		t.Errorf("unexpected message: %s", result.Streams[0].Entries[0].Message)
	}
}

func TestWriteQueryTimeRange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now()

	entries := []logging.LogEntry{
		{
			Timestamp: base,
			Level:     "info",
			Message:   "entry-1",
			Labels:    map[string]string{"source": "worker"},
		},
		{
			Timestamp: base.Add(10 * time.Second),
			Level:     "info",
			Message:   "entry-2",
			Labels:    map[string]string{"source": "worker"},
		},
		{
			Timestamp: base.Add(20 * time.Second),
			Level:     "info",
			Message:   "entry-3",
			Labels:    map[string]string{"source": "worker"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	// Query only the middle entry.
	result, err := s.Query(ctx, logging.QueryRequest{
		Query:     `{source="worker"}`,
		Start:     base.Add(5 * time.Second),
		End:       base.Add(15 * time.Second),
		Limit:     100,
		Direction: "forward",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(result.Streams))
	}
	if len(result.Streams[0].Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result.Streams[0].Entries))
	}
	if result.Streams[0].Entries[0].Message != "entry-2" {
		t.Errorf("expected entry-2, got %s", result.Streams[0].Entries[0].Message)
	}
}

func TestListLabelsAndValues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entries := []logging.LogEntry{
		{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   "test",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
		{
			Timestamp: time.Now(),
			Level:     "warn",
			Message:   "test2",
			Labels:    map[string]string{"source": "worker", "type": "service"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	labels, err := s.ListLabels(ctx)
	if err != nil {
		t.Fatal(err)
	}

	labelSet := make(map[string]bool)
	for _, l := range labels {
		labelSet[l] = true
	}
	if !labelSet["source"] || !labelSet["type"] {
		t.Errorf("expected source and type labels, got %v", labels)
	}

	values, err := s.ListLabelValues(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}

	valueSet := make(map[string]bool)
	for _, v := range values {
		valueSet[v] = true
	}
	if !valueSet["cp"] || !valueSet["worker"] {
		t.Errorf("expected cp and worker values, got %v", values)
	}
}

func TestFindStreamsExactMatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entries := []logging.LogEntry{
		{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   "from cp",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
		{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   "from worker",
			Labels:    map[string]string{"source": "worker", "type": "service"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	ids, err := s.FindStreams(ctx, []LabelMatcher{
		{Name: "source", Value: "cp", Type: MatchEqual},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(ids))
	}

	expectedID := entries[0].StreamID()
	if ids[0] != expectedID {
		t.Errorf("expected stream ID %d, got %d", expectedID, ids[0])
	}
}

func TestFindStreamsRegex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	entries := []logging.LogEntry{
		{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   "from cp",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
		{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   "from worker",
			Labels:    map[string]string{"source": "worker", "type": "service"},
		},
		{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   "from gateway",
			Labels:    map[string]string{"source": "gateway", "type": "system"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	// Match sources starting with "c" or "g".
	ids, err := s.FindStreams(ctx, []LabelMatcher{
		{Name: "source", Value: "cp|gateway", Type: MatchRegexp},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(ids))
	}
}

func TestTail(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Tail(ctx, `{source="cp"}`)
	if err != nil {
		t.Fatal(err)
	}

	// Write an entry after subscribing.
	entry := logging.LogEntry{
		Timestamp: time.Now(),
		Level:     "info",
		Message:   "live log",
		Labels:    map[string]string{"source": "cp"},
	}

	if err := s.Write(context.Background(), []logging.LogEntry{entry}); err != nil {
		t.Fatal(err)
	}

	// Should receive the entry on the channel.
	select {
	case received := <-ch:
		if received.Message != "live log" {
			t.Errorf("expected 'live log', got %q", received.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tail entry")
	}
}

func TestKeyEncodingRoundtrip(t *testing.T) {
	t.Run("LogEntryKey", func(t *testing.T) {
		streamID := uint64(123456789)
		tsNs := time.Now().UnixNano()
		key := EncodeLogEntryKey(streamID, tsNs)
		gotStream, gotTs, ok := DecodeLogEntryKey(key)
		if !ok {
			t.Fatal("decode failed")
		}
		if gotStream != streamID {
			t.Errorf("stream ID: got %d, want %d", gotStream, streamID)
		}
		if gotTs != tsNs {
			t.Errorf("timestamp: got %d, want %d", gotTs, tsNs)
		}
	})

	t.Run("StreamKey", func(t *testing.T) {
		streamID := uint64(987654321)
		key := EncodeStreamKey(streamID)
		got, ok := DecodeStreamKey(key)
		if !ok {
			t.Fatal("decode failed")
		}
		if got != streamID {
			t.Errorf("stream ID: got %d, want %d", got, streamID)
		}
	})

	t.Run("InvertedKey", func(t *testing.T) {
		name := "source"
		value := "worker"
		streamID := uint64(42)
		key := EncodeInvertedKey(name, value, streamID)
		gotName, gotValue, gotStream, ok := DecodeInvertedKey(key)
		if !ok {
			t.Fatal("decode failed")
		}
		if gotName != name {
			t.Errorf("name: got %q, want %q", gotName, name)
		}
		if gotValue != value {
			t.Errorf("value: got %q, want %q", gotValue, value)
		}
		if gotStream != streamID {
			t.Errorf("stream ID: got %d, want %d", gotStream, streamID)
		}
	})

	t.Run("LabelNameKey", func(t *testing.T) {
		name := "my_label"
		key := EncodeLabelNameKey(name)
		got, ok := DecodeLabelNameKey(key)
		if !ok {
			t.Fatal("decode failed")
		}
		if got != name {
			t.Errorf("name: got %q, want %q", got, name)
		}
	})
}

func TestQueryWithMatchers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entries := []logging.LogEntry{
		{
			Timestamp: now,
			Level:     "info",
			Message:   "cp log alpha",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
		{
			Timestamp: now.Add(time.Second),
			Level:     "info",
			Message:   "worker log beta",
			Labels:    map[string]string{"source": "worker", "type": "service"},
		},
		{
			Timestamp: now.Add(2 * time.Second),
			Level:     "error",
			Message:   "cp error gamma",
			Labels:    map[string]string{"source": "cp", "type": "system"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	// Find only "cp" streams.
	streamIDs, err := s.FindStreams(ctx, []LabelMatcher{
		{Name: "source", Value: "cp", Type: MatchEqual},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(streamIDs) != 1 {
		t.Fatalf("expected 1 cp stream, got %d", len(streamIDs))
	}

	// Query with matchers, filter lines containing "error".
	result, err := s.QueryWithMatchers(ctx, streamIDs, now.Add(-time.Minute), now.Add(time.Minute), 100, "forward", func(msg string) bool {
		return strings.Contains(msg, "error")
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(result.Streams))
	}
	if len(result.Streams[0].Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result.Streams[0].Entries))
	}
	if result.Streams[0].Entries[0].Message != "cp error gamma" {
		t.Errorf("unexpected message: %s", result.Streams[0].Entries[0].Message)
	}

	// Query with nil lineFilter to get all cp entries.
	resultAll, err := s.QueryWithMatchers(ctx, streamIDs, now.Add(-time.Minute), now.Add(time.Minute), 100, "forward", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resultAll.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(resultAll.Streams))
	}
	if len(resultAll.Streams[0].Entries) != 2 {
		t.Fatalf("expected 2 entries without filter, got %d", len(resultAll.Streams[0].Entries))
	}
}

func TestTailContextCancel(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := s.Tail(ctx, `{source="cp"}`)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the context.
	cancel()

	// The channel should eventually be closed (drain any pending items first).
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// Channel closed as expected.
				return
			}
			// Received an entry; keep draining.
		case <-timer.C:
			t.Fatal("timed out waiting for tail channel to close after context cancel")
		}
	}
}

func TestQueryWithLineFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entries := []logging.LogEntry{
		{
			Timestamp: now,
			Level:     "info",
			Message:   "request started",
			Labels:    map[string]string{"source": "cp"},
		},
		{
			Timestamp: now.Add(time.Second),
			Level:     "error",
			Message:   "request failed with error",
			Labels:    map[string]string{"source": "cp"},
		},
		{
			Timestamp: now.Add(2 * time.Second),
			Level:     "info",
			Message:   "request completed",
			Labels:    map[string]string{"source": "cp"},
		},
	}

	if err := s.Write(ctx, entries); err != nil {
		t.Fatal(err)
	}

	streamIDs, err := s.FindStreams(ctx, []LabelMatcher{
		{Name: "source", Value: "cp", Type: MatchEqual},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Filter only entries containing "error".
	result, err := s.QueryWithMatchers(ctx, streamIDs, now.Add(-time.Minute), now.Add(time.Minute), 100, "forward", func(msg string) bool {
		return strings.Contains(msg, "error")
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(result.Streams))
	}
	if len(result.Streams[0].Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result.Streams[0].Entries))
	}
	if result.Streams[0].Entries[0].Message != "request failed with error" {
		t.Errorf("unexpected message: %s", result.Streams[0].Entries[0].Message)
	}
}
