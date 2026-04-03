package logging

import (
	"testing"
)

func TestStreamIDDeterministic(t *testing.T) {
	labels := map[string]string{"source": "cp", "type": "system", "level": "info"}
	e1 := LogEntry{Labels: labels}
	e2 := LogEntry{Labels: labels}
	if e1.StreamID() != e2.StreamID() {
		t.Errorf("same labels produced different IDs: %d vs %d", e1.StreamID(), e2.StreamID())
	}
}

func TestStreamIDDifferent(t *testing.T) {
	e1 := LogEntry{Labels: map[string]string{"source": "cp"}}
	e2 := LogEntry{Labels: map[string]string{"source": "worker"}}
	if e1.StreamID() == e2.StreamID() {
		t.Error("different labels should produce different stream IDs")
	}
}

func TestStreamIDOrderIndependent(t *testing.T) {
	// Build maps in different insertion order (Go maps are unordered, but
	// StreamID sorts keys, so the result should be the same).
	labels1 := map[string]string{"a": "1", "b": "2", "c": "3"}
	labels2 := map[string]string{"c": "3", "a": "1", "b": "2"}

	e1 := LogEntry{Labels: labels1}
	e2 := LogEntry{Labels: labels2}
	if e1.StreamID() != e2.StreamID() {
		t.Errorf("order-independent labels produced different IDs: %d vs %d", e1.StreamID(), e2.StreamID())
	}
}

func TestStreamKey(t *testing.T) {
	e := LogEntry{Labels: map[string]string{"b": "2", "a": "1"}}
	key := e.StreamKey()

	// Labels should be sorted: a first, then b.
	expected := `{a="1", b="2"}`
	if key != expected {
		t.Errorf("got %q, want %q", key, expected)
	}
}
