package tsdb

import (
	"math"
	"testing"
)

func TestDataPointKeyRoundtrip(t *testing.T) {
	cases := []struct {
		seriesID    uint64
		timestampMs int64
	}{
		{1, 1000},
		{0, 0},
		{math.MaxUint64, math.MaxInt64},
		{42, -1},
		{123456789, 1711929600000},
	}
	for _, tc := range cases {
		key := EncodeDataPointKey(tc.seriesID, tc.timestampMs)
		sid, ts, ok := DecodeDataPointKey(key)
		if !ok {
			t.Fatalf("DecodeDataPointKey failed for seriesID=%d ts=%d", tc.seriesID, tc.timestampMs)
		}
		if sid != tc.seriesID {
			t.Errorf("seriesID: got %d, want %d", sid, tc.seriesID)
		}
		if ts != tc.timestampMs {
			t.Errorf("timestampMs: got %d, want %d", ts, tc.timestampMs)
		}
	}
}

func TestSeriesKeyRoundtrip(t *testing.T) {
	for _, id := range []uint64{0, 1, 999, math.MaxUint64} {
		key := EncodeSeriesKey(id)
		got, ok := DecodeSeriesKey(key)
		if !ok {
			t.Fatalf("DecodeSeriesKey failed for %d", id)
		}
		if got != id {
			t.Errorf("got %d, want %d", got, id)
		}
	}
}

func TestInvertedKeyRoundtrip(t *testing.T) {
	cases := []struct {
		name, value string
		seriesID    uint64
	}{
		{"__name__", "cpu_usage", 1},
		{"job", "prometheus", 42},
		{"", "", 0},
		{"instance", "localhost:9090", math.MaxUint64},
	}
	for _, tc := range cases {
		key := EncodeInvertedKey(tc.name, tc.value, tc.seriesID)
		n, v, sid, ok := DecodeInvertedKey(key)
		if !ok {
			t.Fatalf("DecodeInvertedKey failed for %s=%s sid=%d", tc.name, tc.value, tc.seriesID)
		}
		if n != tc.name {
			t.Errorf("labelName: got %q, want %q", n, tc.name)
		}
		if v != tc.value {
			t.Errorf("labelValue: got %q, want %q", v, tc.value)
		}
		if sid != tc.seriesID {
			t.Errorf("seriesID: got %d, want %d", sid, tc.seriesID)
		}
	}
}

func TestMetricNameKeyRoundtrip(t *testing.T) {
	for _, name := range []string{"cpu_usage", "http_requests_total", "go_goroutines"} {
		key := EncodeMetricNameKey(name)
		got, ok := DecodeMetricNameKey(key)
		if !ok {
			t.Fatalf("DecodeMetricNameKey failed for %q", name)
		}
		if got != name {
			t.Errorf("got %q, want %q", got, name)
		}
	}
}

func TestFloat64Roundtrip(t *testing.T) {
	values := []float64{
		0, 1.5, -1.5, math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.Inf(1), math.Inf(-1),
	}
	for _, v := range values {
		buf := EncodeFloat64(v)
		got := DecodeFloat64(buf)
		if got != v {
			t.Errorf("got %v, want %v", got, v)
		}
	}

	// NaN special case: NaN != NaN, so check with IsNaN.
	buf := EncodeFloat64(math.NaN())
	got := DecodeFloat64(buf)
	if !math.IsNaN(got) {
		t.Errorf("expected NaN, got %v", got)
	}
}

func TestDecodeInvalidKeys(t *testing.T) {
	// Empty key.
	if _, _, ok := DecodeDataPointKey(nil); ok {
		t.Error("expected false for nil data point key")
	}
	if _, ok := DecodeSeriesKey(nil); ok {
		t.Error("expected false for nil series key")
	}
	if _, _, _, ok := DecodeInvertedKey(nil); ok {
		t.Error("expected false for nil inverted key")
	}
	if _, ok := DecodeMetricNameKey(nil); ok {
		t.Error("expected false for nil metric name key")
	}

	// Wrong prefix.
	if _, _, ok := DecodeDataPointKey([]byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("expected false for wrong prefix on data point key")
	}
	if _, ok := DecodeSeriesKey([]byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("expected false for wrong prefix on series key")
	}

	// Too short.
	if _, _, ok := DecodeDataPointKey([]byte{0x01, 0, 0}); ok {
		t.Error("expected false for short data point key")
	}
	if _, ok := DecodeSeriesKey([]byte{0x02, 0}); ok {
		t.Error("expected false for short series key")
	}
	if _, ok := DecodeMetricNameKey([]byte{0x04}); ok {
		t.Error("expected false for single-byte metric name key")
	}
}
