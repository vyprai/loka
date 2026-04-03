package handler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/logging"
)

// mockWriter records all entries written to it.
type mockWriter struct {
	mu      sync.Mutex
	entries []logging.LogEntry
}

func (m *mockWriter) Write(_ context.Context, entries []logging.LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *mockWriter) getEntries() []logging.LogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]logging.LogEntry, len(m.entries))
	copy(cp, m.entries)
	return cp
}

// mockInner is a minimal slog.Handler that records whether Handle was called.
type mockInner struct {
	mu      sync.Mutex
	handled []slog.Record
}

func (m *mockInner) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (m *mockInner) Handle(_ context.Context, r slog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handled = append(m.handled, r)
	return nil
}
func (m *mockInner) WithAttrs(attrs []slog.Attr) slog.Handler { return m }
func (m *mockInner) WithGroup(name string) slog.Handler       { return m }

func (m *mockInner) getHandled() []slog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]slog.Record, len(m.handled))
	copy(cp, m.handled)
	return cp
}

func TestHandlerWritesToBothInnerAndWriter(t *testing.T) {
	inner := &mockInner{}
	writer := &mockWriter{}
	h := New(writer, map[string]string{"source": "cp"}, inner)
	defer h.Close()

	logger := slog.New(h)
	logger.Info("hello")

	h.Close()

	if len(inner.getHandled()) != 1 {
		t.Fatalf("expected inner to handle 1 record, got %d", len(inner.getHandled()))
	}

	entries := writer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected writer to receive 1 entry, got %d", len(entries))
	}
}

func TestHandlerConvertsRecordToLogEntry(t *testing.T) {
	writer := &mockWriter{}
	inner := &mockInner{}
	h := New(writer, map[string]string{"source": "cp"}, inner)

	now := time.Now()
	record := slog.NewRecord(now, slog.LevelInfo, "test message", 0)
	h.Handle(context.Background(), record)
	h.Close()

	entries := writer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Message != "test message" {
		t.Errorf("expected message 'test message', got %q", e.Message)
	}
	if e.Level != "info" {
		t.Errorf("expected level 'info', got %q", e.Level)
	}
	if !e.Timestamp.Equal(now) {
		t.Errorf("expected timestamp %v, got %v", now, e.Timestamp)
	}
}

func TestHandlerIncludesBaseLabels(t *testing.T) {
	writer := &mockWriter{}
	inner := &mockInner{}
	labels := map[string]string{"source": "cp", "type": "system"}
	h := New(writer, labels, inner)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	h.Handle(context.Background(), record)
	h.Close()

	entries := writer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Labels["source"] != "cp" {
		t.Errorf("expected label source=cp, got %q", e.Labels["source"])
	}
	if e.Labels["type"] != "system" {
		t.Errorf("expected label type=system, got %q", e.Labels["type"])
	}
	if e.Labels["level"] != "info" {
		t.Errorf("expected label level=info, got %q", e.Labels["level"])
	}
}

func TestHandlerExtractsAttrsAsFields(t *testing.T) {
	writer := &mockWriter{}
	inner := &mockInner{}
	h := New(writer, nil, inner)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	record.AddAttrs(slog.String("service", "web"), slog.Int("port", 8080))
	h.Handle(context.Background(), record)
	h.Close()

	entries := writer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Fields["service"] != "web" {
		t.Errorf("expected field service=web, got %q", entries[0].Fields["service"])
	}
	if entries[0].Fields["port"] != "8080" {
		t.Errorf("expected field port=8080, got %q", entries[0].Fields["port"])
	}
}

func TestHandlerBatchesWrites(t *testing.T) {
	writer := &mockWriter{}
	inner := &mockInner{}
	h := New(writer, nil, inner)

	// Send multiple records.
	for i := 0; i < 50; i++ {
		record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
		h.Handle(context.Background(), record)
	}

	h.Close()

	entries := writer.getEntries()
	if len(entries) != 50 {
		t.Errorf("expected 50 entries written, got %d", len(entries))
	}
}

func TestWithAttrsAccumulatesAttrs(t *testing.T) {
	writer := &mockWriter{}
	inner := &mockInner{}
	h := New(writer, nil, inner)

	h2 := h.WithAttrs([]slog.Attr{slog.String("component", "api")})

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	h2.Handle(context.Background(), record)
	h.Close()

	entries := writer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Fields["component"] != "api" {
		t.Errorf("expected field component=api, got %q", entries[0].Fields["component"])
	}
}

func TestNilWriterNoPanic(t *testing.T) {
	inner := &mockInner{}
	h := New(nil, map[string]string{"source": "cp"}, inner)
	defer h.Close()

	logger := slog.New(h)
	logger.Info("should not panic")

	if len(inner.getHandled()) != 1 {
		t.Fatalf("expected inner to handle 1 record, got %d", len(inner.getHandled()))
	}
}
