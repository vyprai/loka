package audit

import (
	"context"
	"sync"
	"testing"

	"github.com/vyprai/loka/internal/controlplane/logging"
)

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

func TestLogCreatesEntryWithCorrectLabels(t *testing.T) {
	w := &mockWriter{}
	l := New(w)

	l.Log(context.Background(), "exec", "admin", "service/web", "success", nil)

	entries := w.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Labels["source"] != "cp" {
		t.Errorf("expected label source=cp, got %q", e.Labels["source"])
	}
	if e.Labels["type"] != "audit" {
		t.Errorf("expected label type=audit, got %q", e.Labels["type"])
	}
	if e.Labels["action"] != "exec" {
		t.Errorf("expected label action=exec, got %q", e.Labels["action"])
	}
	if e.Labels["level"] != "info" {
		t.Errorf("expected label level=info, got %q", e.Labels["level"])
	}
}

func TestLogFormatsMessageCorrectly(t *testing.T) {
	w := &mockWriter{}
	l := New(w)

	l.Log(context.Background(), "deploy", "user1", "service/api", "completed", nil)

	entries := w.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	expected := "user1 deploy service/api: completed"
	if entries[0].Message != expected {
		t.Errorf("expected message %q, got %q", expected, entries[0].Message)
	}
}

func TestLogIncludesDetailsAsFields(t *testing.T) {
	w := &mockWriter{}
	l := New(w)

	details := map[string]string{"ip": "10.0.0.1", "method": "POST"}
	l.Log(context.Background(), "exec", "admin", "service/web", "success", details)

	entries := w.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Fields["ip"] != "10.0.0.1" {
		t.Errorf("expected field ip=10.0.0.1, got %q", entries[0].Fields["ip"])
	}
	if entries[0].Fields["method"] != "POST" {
		t.Errorf("expected field method=POST, got %q", entries[0].Fields["method"])
	}
}

func TestNilWriterNoPanic(t *testing.T) {
	l := New(nil)
	// Should not panic.
	l.Log(context.Background(), "exec", "admin", "service/web", "success", nil)
}
