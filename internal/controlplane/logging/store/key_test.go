package store

import (
	"math"
	"testing"
)

func TestLogEntryKeyRoundtrip(t *testing.T) {
	cases := []struct {
		streamID    uint64
		timestampNs int64
	}{
		{1, 1000},
		{0, 0},
		{math.MaxUint64, math.MaxInt64},
		{42, -1},
		{99, 1711929600000000000}, // ~2024 in nanoseconds
	}
	for _, tc := range cases {
		key := EncodeLogEntryKey(tc.streamID, tc.timestampNs)
		sid, ts, ok := DecodeLogEntryKey(key)
		if !ok {
			t.Fatalf("DecodeLogEntryKey failed for streamID=%d ts=%d", tc.streamID, tc.timestampNs)
		}
		if sid != tc.streamID {
			t.Errorf("streamID: got %d, want %d", sid, tc.streamID)
		}
		if ts != tc.timestampNs {
			t.Errorf("timestampNs: got %d, want %d", ts, tc.timestampNs)
		}
	}
}

func TestStreamKeyRoundtrip(t *testing.T) {
	for _, id := range []uint64{0, 1, 999, math.MaxUint64} {
		key := EncodeStreamKey(id)
		got, ok := DecodeStreamKey(key)
		if !ok {
			t.Fatalf("DecodeStreamKey failed for %d", id)
		}
		if got != id {
			t.Errorf("got %d, want %d", got, id)
		}
	}
}

func TestLogInvertedKeyRoundtrip(t *testing.T) {
	cases := []struct {
		name, value string
		streamID    uint64
	}{
		{"source", "cp", 1},
		{"type", "service", 42},
		{"", "", 0},
		{"worker_id", "w-abc-123", math.MaxUint64},
	}
	for _, tc := range cases {
		key := EncodeInvertedKey(tc.name, tc.value, tc.streamID)
		n, v, sid, ok := DecodeInvertedKey(key)
		if !ok {
			t.Fatalf("DecodeInvertedKey failed for %s=%s sid=%d", tc.name, tc.value, tc.streamID)
		}
		if n != tc.name {
			t.Errorf("labelName: got %q, want %q", n, tc.name)
		}
		if v != tc.value {
			t.Errorf("labelValue: got %q, want %q", v, tc.value)
		}
		if sid != tc.streamID {
			t.Errorf("streamID: got %d, want %d", sid, tc.streamID)
		}
	}
}

func TestLabelNameKeyRoundtrip(t *testing.T) {
	for _, name := range []string{"source", "type", "worker_id", "level"} {
		key := EncodeLabelNameKey(name)
		got, ok := DecodeLabelNameKey(key)
		if !ok {
			t.Fatalf("DecodeLabelNameKey failed for %q", name)
		}
		if got != name {
			t.Errorf("got %q, want %q", got, name)
		}
	}
}

func TestLogDecodeInvalidKeys(t *testing.T) {
	// Wrong prefix.
	if _, _, ok := DecodeLogEntryKey([]byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("expected false for wrong prefix on log entry key")
	}
	if _, ok := DecodeStreamKey([]byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("expected false for wrong prefix on stream key")
	}

	// Too short.
	if _, _, ok := DecodeLogEntryKey([]byte{0x10, 0, 0}); ok {
		t.Error("expected false for short log entry key")
	}
	if _, ok := DecodeStreamKey([]byte{0x11, 0}); ok {
		t.Error("expected false for short stream key")
	}
	if _, ok := DecodeLabelNameKey([]byte{0x13}); ok {
		t.Error("expected false for single-byte label name key")
	}

	// Nil keys.
	if _, _, ok := DecodeLogEntryKey(nil); ok {
		t.Error("expected false for nil log entry key")
	}
	if _, ok := DecodeStreamKey(nil); ok {
		t.Error("expected false for nil stream key")
	}
	if _, _, _, ok := DecodeInvertedKey(nil); ok {
		t.Error("expected false for nil inverted key")
	}
	if _, ok := DecodeLabelNameKey(nil); ok {
		t.Error("expected false for nil label name key")
	}
}
