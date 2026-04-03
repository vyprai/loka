package tsdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/metrics"
)

func newTestStore(t *testing.T) MetricsStore {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{
		DataDir:   dir,
		Retention: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestWriteAndQuery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	points := []metrics.DataPoint{
		{
			Name:      "vm_cpu_usage_percent",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "id", Value: "service_1"}, {Name: "type", Value: "service"}},
			Timestamp: now.Add(-2 * time.Minute).UnixMilli(),
			Value:     42.5,
		},
		{
			Name:      "vm_cpu_usage_percent",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "id", Value: "service_1"}, {Name: "type", Value: "service"}},
			Timestamp: now.Add(-1 * time.Minute).UnixMilli(),
			Value:     55.0,
		},
		{
			Name:      "vm_cpu_usage_percent",
			Type:      metrics.Gauge,
			Labels:    metrics.Labels{{Name: "id", Value: "service_2"}, {Name: "type", Value: "service"}},
			Timestamp: now.Add(-1 * time.Minute).UnixMilli(),
			Value:     28.1,
		},
	}

	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Query for service_1.
	seriesID := points[0].SeriesID()
	results, err := store.Query(ctx, QueryRequest{
		SeriesIDs: []uint64{seriesID},
		Start:     now.Add(-5 * time.Minute),
		End:       now,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].Points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(results[0].Points))
	}
	if results[0].Points[0].Value != 42.5 {
		t.Fatalf("expected 42.5, got %f", results[0].Points[0].Value)
	}
	if results[0].Points[1].Value != 55.0 {
		t.Fatalf("expected 55.0, got %f", results[0].Points[1].Value)
	}
}

func TestListMetrics(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	points := []metrics.DataPoint{
		{Name: "metric_a", Type: metrics.Counter, Timestamp: time.Now().UnixMilli(), Value: 1},
		{Name: "metric_b", Type: metrics.Gauge, Timestamp: time.Now().UnixMilli(), Value: 2},
		{Name: "metric_a", Type: metrics.Counter, Timestamp: time.Now().UnixMilli(), Value: 3},
	}

	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	names, err := store.ListMetrics(ctx)
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["metric_a"] || !nameSet["metric_b"] {
		t.Fatalf("expected metric_a and metric_b, got %v", names)
	}
}

func TestFindSeriesExactMatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	points := []metrics.DataPoint{
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "service"}}, Timestamp: time.Now().UnixMilli(), Value: 1},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "task"}}, Timestamp: time.Now().UnixMilli(), Value: 2},
		{Name: "mem", Labels: metrics.Labels{{Name: "type", Value: "service"}}, Timestamp: time.Now().UnixMilli(), Value: 3},
	}
	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Find cpu + type=service.
	ids, err := store.FindSeries(ctx, []LabelMatcher{
		{Name: "__name__", Value: "cpu", Type: MatchEqual},
		{Name: "type", Value: "service", Type: MatchEqual},
	})
	if err != nil {
		t.Fatalf("FindSeries: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 series, got %d", len(ids))
	}
	if ids[0] != points[0].SeriesID() {
		t.Fatalf("expected series ID %d, got %d", points[0].SeriesID(), ids[0])
	}
}

func TestFindSeriesRegex(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	points := []metrics.DataPoint{
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "service"}}, Timestamp: time.Now().UnixMilli(), Value: 1},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "session"}}, Timestamp: time.Now().UnixMilli(), Value: 2},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "task"}}, Timestamp: time.Now().UnixMilli(), Value: 3},
	}
	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Find type=~"service|session".
	ids, err := store.FindSeries(ctx, []LabelMatcher{
		{Name: "__name__", Value: "cpu", Type: MatchEqual},
		{Name: "type", Value: "service|session", Type: MatchRegexp},
	})
	if err != nil {
		t.Fatalf("FindSeries: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 series, got %d", len(ids))
	}
}

func TestFindSeriesNotEqual(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	points := []metrics.DataPoint{
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "service"}}, Timestamp: time.Now().UnixMilli(), Value: 1},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "task"}}, Timestamp: time.Now().UnixMilli(), Value: 2},
	}
	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Find type != "task".
	ids, err := store.FindSeries(ctx, []LabelMatcher{
		{Name: "__name__", Value: "cpu", Type: MatchEqual},
		{Name: "type", Value: "task", Type: MatchNotEqual},
	})
	if err != nil {
		t.Fatalf("FindSeries: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 series, got %d", len(ids))
	}
}

func TestGetSeriesInfo(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	dp := metrics.DataPoint{
		Name:      "cpu",
		Type:      metrics.Gauge,
		Labels:    metrics.Labels{{Name: "id", Value: "svc_1"}, {Name: "type", Value: "service"}},
		Timestamp: time.Now().UnixMilli(),
		Value:     42.5,
	}
	if err := store.Write(ctx, []metrics.DataPoint{dp}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := store.GetSeriesInfo(ctx, dp.SeriesID())
	if err != nil {
		t.Fatalf("GetSeriesInfo: %v", err)
	}
	if info.Name != "cpu" {
		t.Fatalf("expected name cpu, got %s", info.Name)
	}
	if info.Labels["type"] != "service" {
		t.Fatalf("expected type=service, got %v", info.Labels)
	}
}

func TestListLabelNamesAndValues(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	points := []metrics.DataPoint{
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "service"}, {Name: "id", Value: "s1"}}, Timestamp: time.Now().UnixMilli(), Value: 1},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "task"}, {Name: "id", Value: "t1"}}, Timestamp: time.Now().UnixMilli(), Value: 2},
	}
	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	names, err := store.ListLabelNames(ctx)
	if err != nil {
		t.Fatalf("ListLabelNames: %v", err)
	}
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["type"] || !nameSet["id"] || !nameSet["__name__"] {
		t.Fatalf("expected type, id, __name__, got %v", names)
	}

	values, err := store.ListLabelValues(ctx, "type")
	if err != nil {
		t.Fatalf("ListLabelValues: %v", err)
	}
	valSet := make(map[string]bool)
	for _, v := range values {
		valSet[v] = true
	}
	if !valSet["service"] || !valSet["task"] {
		t.Fatalf("expected service and task, got %v", values)
	}
}

func TestQueryWithDownsampling(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// Write 10 points, 1 per minute.
	dp := metrics.DataPoint{
		Name:   "cpu",
		Type:   metrics.Gauge,
		Labels: metrics.Labels{{Name: "id", Value: "s1"}},
	}
	var points []metrics.DataPoint
	for i := 0; i < 10; i++ {
		p := dp
		p.Timestamp = now.Add(-time.Duration(10-i) * time.Minute).UnixMilli()
		p.Value = float64(i * 10)
		points = append(points, p)
	}
	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Query with 5-minute step.
	results, err := store.Query(ctx, QueryRequest{
		SeriesIDs: []uint64{dp.SeriesID()},
		Start:     now.Add(-10 * time.Minute),
		End:       now,
		Step:      5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	// Should have ~2 buckets.
	if len(results[0].Points) < 2 {
		t.Fatalf("expected at least 2 downsampled points, got %d", len(results[0].Points))
	}
}

func TestKeyEncodingRoundtrip(t *testing.T) {
	// Data point key.
	var seriesID uint64 = 12345678901234
	var ts int64 = 1743465600000
	key := EncodeDataPointKey(seriesID, ts)
	gotSID, gotTS, ok := DecodeDataPointKey(key)
	if !ok || gotSID != seriesID || gotTS != ts {
		t.Fatalf("DataPointKey roundtrip failed: got (%d, %d, %v)", gotSID, gotTS, ok)
	}

	// Series key.
	sKey := EncodeSeriesKey(seriesID)
	gotSID2, ok := DecodeSeriesKey(sKey)
	if !ok || gotSID2 != seriesID {
		t.Fatalf("SeriesKey roundtrip failed")
	}

	// Inverted key.
	iKey := EncodeInvertedKey("type", "service", seriesID)
	gotName, gotVal, gotSID3, ok := DecodeInvertedKey(iKey)
	if !ok || gotName != "type" || gotVal != "service" || gotSID3 != seriesID {
		t.Fatalf("InvertedKey roundtrip failed: %q %q %d %v", gotName, gotVal, gotSID3, ok)
	}

	// Metric name key.
	mnKey := EncodeMetricNameKey("vm_cpu_usage_percent")
	gotMN, ok := DecodeMetricNameKey(mnKey)
	if !ok || gotMN != "vm_cpu_usage_percent" {
		t.Fatalf("MetricNameKey roundtrip failed")
	}

	// Float64.
	val := 42.123456789
	encoded := EncodeFloat64(val)
	decoded := DecodeFloat64(encoded)
	if decoded != val {
		t.Fatalf("Float64 roundtrip failed: %f != %f", decoded, val)
	}
}

func TestFindSeriesNotRegexp(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	points := []metrics.DataPoint{
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "service"}}, Timestamp: time.Now().UnixMilli(), Value: 1},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "session"}}, Timestamp: time.Now().UnixMilli(), Value: 2},
		{Name: "cpu", Labels: metrics.Labels{{Name: "type", Value: "task"}}, Timestamp: time.Now().UnixMilli(), Value: 3},
	}
	if err := store.Write(ctx, points); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Find type !~ "service|session" → only task.
	ids, err := store.FindSeries(ctx, []LabelMatcher{
		{Name: "__name__", Value: "cpu", Type: MatchEqual},
		{Name: "type", Value: "service|session", Type: MatchNotRegexp},
	})
	if err != nil {
		t.Fatalf("FindSeries: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 series, got %d", len(ids))
	}
}

func TestConcurrentWriteQuery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Write from multiple goroutines while querying.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			dp := metrics.DataPoint{
				Name:      "concurrent",
				Labels:    metrics.Labels{{Name: "i", Value: fmt.Sprintf("%d", i)}},
				Timestamp: time.Now().UnixMilli(),
				Value:     float64(i),
			}
			store.Write(ctx, []metrics.DataPoint{dp})
		}
	}()

	// Query while writes are happening.
	for i := 0; i < 10; i++ {
		store.ListMetrics(ctx)
		store.ListLabelNames(ctx)
	}

	<-done
}

func TestStatsTracking(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	dp := metrics.DataPoint{Name: "test", Timestamp: time.Now().UnixMilli(), Value: 1}
	store.Write(ctx, []metrics.DataPoint{dp})

	stats := store.GetStats()
	if stats.WriteSamplesTotal.Load() != 1 {
		t.Errorf("expected 1 write sample, got %d", stats.WriteSamplesTotal.Load())
	}

	store.Query(ctx, QueryRequest{SeriesIDs: []uint64{dp.SeriesID()}, Start: time.Now().Add(-time.Hour), End: time.Now()})
	if stats.QueryTotal.Load() != 1 {
		t.Errorf("expected 1 query, got %d", stats.QueryTotal.Load())
	}
}
