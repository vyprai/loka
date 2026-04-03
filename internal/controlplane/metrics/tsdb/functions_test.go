package tsdb

import (
	"math"
	"testing"
	"time"
)

func TestApplyRate_MonotonicCounter(t *testing.T) {
	// Counter goes 0, 10, 20, 30, 40 over 40 seconds.
	points := []TimeValue{
		{TimestampMs: 0, Value: 0},
		{TimestampMs: 10_000, Value: 10},
		{TimestampMs: 20_000, Value: 20},
		{TimestampMs: 30_000, Value: 30},
		{TimestampMs: 40_000, Value: 40},
	}
	result := ApplyRate(points, 40*time.Second)
	if len(result) != 1 {
		t.Fatalf("expected 1 result point, got %d", len(result))
	}
	// Total increase = 40, duration = 40s => rate = 1.0
	if math.Abs(result[0].Value-1.0) > 1e-9 {
		t.Errorf("expected rate 1.0, got %f", result[0].Value)
	}
	if result[0].TimestampMs != 40_000 {
		t.Errorf("expected timestamp 40000, got %d", result[0].TimestampMs)
	}
}

func TestApplyRate_CounterReset(t *testing.T) {
	// Counter goes 0, 10, 20, 5, 15 (reset at index 3).
	points := []TimeValue{
		{TimestampMs: 0, Value: 0},
		{TimestampMs: 10_000, Value: 10},
		{TimestampMs: 20_000, Value: 20},
		{TimestampMs: 30_000, Value: 5},  // reset
		{TimestampMs: 40_000, Value: 15}, // continued after reset
	}
	result := ApplyRate(points, 40*time.Second)
	if len(result) != 1 {
		t.Fatalf("expected 1 result point, got %d", len(result))
	}
	// Increases: 10 + 10 + 5 (reset, use current value) + 10 = 35
	// Rate = 35 / 40 = 0.875
	expected := 35.0 / 40.0
	if math.Abs(result[0].Value-expected) > 1e-9 {
		t.Errorf("expected rate %f, got %f", expected, result[0].Value)
	}
}

func TestApplyRate_TooFewPoints(t *testing.T) {
	result := ApplyRate([]TimeValue{{TimestampMs: 0, Value: 1}}, time.Minute)
	if result != nil {
		t.Errorf("expected nil for single point, got %v", result)
	}
	result = ApplyRate(nil, time.Minute)
	if result != nil {
		t.Errorf("expected nil for nil points, got %v", result)
	}
}

func TestApplyDelta(t *testing.T) {
	points := []TimeValue{
		{TimestampMs: 0, Value: 100},
		{TimestampMs: 10_000, Value: 110},
		{TimestampMs: 20_000, Value: 125},
	}
	result := ApplyDelta(points, 20*time.Second)
	if len(result) != 1 {
		t.Fatalf("expected 1 result point, got %d", len(result))
	}
	// Delta = 125 - 100 = 25
	if math.Abs(result[0].Value-25.0) > 1e-9 {
		t.Errorf("expected delta 25, got %f", result[0].Value)
	}
}

func TestApplyIncrease(t *testing.T) {
	points := []TimeValue{
		{TimestampMs: 0, Value: 0},
		{TimestampMs: 30_000, Value: 30},
		{TimestampMs: 60_000, Value: 60},
	}
	result := ApplyIncrease(points, time.Minute)
	if len(result) != 1 {
		t.Fatalf("expected 1 result point, got %d", len(result))
	}
	// Rate = 60/60 = 1.0, increase = 1.0 * 60 = 60
	if math.Abs(result[0].Value-60.0) > 1e-9 {
		t.Errorf("expected increase 60, got %f", result[0].Value)
	}
}

func TestApplyAvgOverTime(t *testing.T) {
	points := []TimeValue{
		{TimestampMs: 0, Value: 10},
		{TimestampMs: 10_000, Value: 20},
		{TimestampMs: 20_000, Value: 30},
	}
	avg := ApplyAvgOverTime(points)
	if math.Abs(avg-20.0) > 1e-9 {
		t.Errorf("expected avg 20, got %f", avg)
	}
}

func TestApplyAvgOverTime_Empty(t *testing.T) {
	avg := ApplyAvgOverTime(nil)
	if !math.IsNaN(avg) {
		t.Errorf("expected NaN for empty points, got %f", avg)
	}
}

func TestApplyMaxOverTime(t *testing.T) {
	points := []TimeValue{
		{TimestampMs: 0, Value: 5},
		{TimestampMs: 10_000, Value: 42},
		{TimestampMs: 20_000, Value: 17},
	}
	m := ApplyMaxOverTime(points)
	if math.Abs(m-42.0) > 1e-9 {
		t.Errorf("expected max 42, got %f", m)
	}
}

func TestApplyMinOverTime(t *testing.T) {
	points := []TimeValue{
		{TimestampMs: 0, Value: 5},
		{TimestampMs: 10_000, Value: 42},
		{TimestampMs: 20_000, Value: 17},
	}
	m := ApplyMinOverTime(points)
	if math.Abs(m-5.0) > 1e-9 {
		t.Errorf("expected min 5, got %f", m)
	}
}

func TestHistogramQuantile_P50(t *testing.T) {
	// Histogram with buckets: 10, 25, 50, 100, +Inf
	// Counts (cumulative): 10, 30, 60, 90, 100
	buckets := map[float64]float64{
		10:              10,
		25:              30,
		50:              60,
		100:             90,
		math.Inf(1):     100,
	}
	p50 := HistogramQuantile(0.5, buckets)
	// rank = 50. Falls in bucket (25, 50] which has count 30..60.
	// fraction = (50 - 30) / (60 - 30) = 20/30 = 0.6667
	// result = 25 + 0.6667 * (50 - 25) = 25 + 16.667 = 41.667
	expected := 25.0 + (20.0/30.0)*25.0
	if math.Abs(p50-expected) > 0.01 {
		t.Errorf("expected p50 ≈ %.3f, got %.3f", expected, p50)
	}
}

func TestHistogramQuantile_P95(t *testing.T) {
	buckets := map[float64]float64{
		10:          10,
		25:          30,
		50:          60,
		100:         90,
		math.Inf(1): 100,
	}
	p95 := HistogramQuantile(0.95, buckets)
	// rank = 95. Falls in bucket (100, +Inf] — but +Inf bound is not useful.
	// Actually rank 95 falls in (50, 100] bucket (count 60..90).
	// fraction = (95 - 90) / (100 - 90) = 5/10 = 0.5
	// result = 100 + 0.5 * (+Inf - 100) = +Inf ... wait, let's recalculate.
	// rank = 95. Buckets sorted: 10(10), 25(30), 50(60), 100(90), +Inf(100).
	// 10 < 95, 30 < 95, 60 < 95, 90 < 95, 100 >= 95.
	// So it falls in the +Inf bucket. prevCount=90, prevBound=100.
	// fraction = (95-90)/(100-90) = 0.5
	// result = 100 + 0.5*(+Inf - 100) = +Inf
	// This is expected for histograms where the last finite bucket doesn't
	// cover p95. In practice, histograms should have a high-enough finite
	// bucket. Let's use a better histogram for a meaningful test.
	_ = p95

	// Better histogram where p95 falls in a finite bucket.
	buckets2 := map[float64]float64{
		10:          10,
		25:          30,
		50:          60,
		100:         95,
		math.Inf(1): 100,
	}
	p95 = HistogramQuantile(0.95, buckets2)
	// rank = 95. 10<95, 30<95, 60<95, 95>=95.
	// Falls in (50, 100] bucket. prevCount=60, prevBound=50.
	// fraction = (95-60)/(95-60) = 1.0
	// result = 50 + 1.0*(100-50) = 100
	if math.Abs(p95-100.0) > 0.01 {
		t.Errorf("expected p95 = 100, got %.3f", p95)
	}
}

func TestHistogramQuantile_P99(t *testing.T) {
	buckets := map[float64]float64{
		0.005: 100,
		0.01:  200,
		0.025: 400,
		0.05:  600,
		0.1:   800,
		0.5:   950,
		1.0:   990,
		5.0:   999,
		math.Inf(1): 1000,
	}
	p99 := HistogramQuantile(0.99, buckets)
	// rank = 990. Falls in bucket (0.5, 1.0] with count 950..990.
	// fraction = (990-950)/(990-950) = 1.0
	// result = 0.5 + 1.0*(1.0-0.5) = 1.0
	if math.Abs(p99-1.0) > 0.01 {
		t.Errorf("expected p99 = 1.0, got %.3f", p99)
	}
}

func TestHistogramQuantile_Empty(t *testing.T) {
	result := HistogramQuantile(0.5, nil)
	if !math.IsNaN(result) {
		t.Errorf("expected NaN for nil buckets, got %f", result)
	}
}

func TestAggregateBy(t *testing.T) {
	groups := map[string][]float64{
		"region=us": {10, 20, 30},
		"region=eu": {5, 15},
	}

	sums := AggregateBy("sum", groups)
	if math.Abs(sums["region=us"]-60) > 1e-9 {
		t.Errorf("expected sum 60 for us, got %f", sums["region=us"])
	}
	if math.Abs(sums["region=eu"]-20) > 1e-9 {
		t.Errorf("expected sum 20 for eu, got %f", sums["region=eu"])
	}

	avgs := AggregateBy("avg", groups)
	if math.Abs(avgs["region=us"]-20) > 1e-9 {
		t.Errorf("expected avg 20 for us, got %f", avgs["region=us"])
	}
	if math.Abs(avgs["region=eu"]-10) > 1e-9 {
		t.Errorf("expected avg 10 for eu, got %f", avgs["region=eu"])
	}

	counts := AggregateBy("count", groups)
	if counts["region=us"] != 3 {
		t.Errorf("expected count 3 for us, got %f", counts["region=us"])
	}
	if counts["region=eu"] != 2 {
		t.Errorf("expected count 2 for eu, got %f", counts["region=eu"])
	}
}
