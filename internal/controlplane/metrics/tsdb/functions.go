package tsdb

import (
	"math"
	"sort"
	"time"
)

// ApplyRate computes the per-second rate of a counter over the given range.
// It handles counter resets: when a value decreases compared to the previous
// point, the decrease is treated as a reset (the counter restarted from 0).
func ApplyRate(points []TimeValue, rangeDuration time.Duration) []TimeValue {
	if len(points) < 2 {
		return nil
	}

	durationSec := rangeDuration.Seconds()
	if durationSec == 0 {
		return nil
	}

	// Walk the points and accumulate the total increase, accounting for resets.
	var totalIncrease float64
	for i := 1; i < len(points); i++ {
		diff := points[i].Value - points[i-1].Value
		if diff >= 0 {
			totalIncrease += diff
		} else {
			// Counter reset: the counter restarted from 0, so the increase
			// for this interval is just the current value.
			totalIncrease += points[i].Value
		}
	}

	rate := totalIncrease / durationSec

	// Return a single point at the last timestamp with the computed rate.
	return []TimeValue{
		{
			TimestampMs: points[len(points)-1].TimestampMs,
			Value:       rate,
		},
	}
}

// ApplyDelta computes the absolute change between the first and last points
// in the window.
func ApplyDelta(points []TimeValue, rangeDuration time.Duration) []TimeValue {
	if len(points) < 2 {
		return nil
	}

	_ = rangeDuration // present for API consistency

	d := points[len(points)-1].Value - points[0].Value

	return []TimeValue{
		{
			TimestampMs: points[len(points)-1].TimestampMs,
			Value:       d,
		},
	}
}

// ApplyIncrease computes the total increase over the range window.
// It equals rate × rangeDuration (in seconds) and handles counter resets
// the same way as ApplyRate.
func ApplyIncrease(points []TimeValue, rangeDuration time.Duration) []TimeValue {
	rated := ApplyRate(points, rangeDuration)
	if len(rated) == 0 {
		return nil
	}

	return []TimeValue{
		{
			TimestampMs: rated[0].TimestampMs,
			Value:       rated[0].Value * rangeDuration.Seconds(),
		},
	}
}

// ApplyAvgOverTime returns the arithmetic mean of all values.
func ApplyAvgOverTime(points []TimeValue) float64 {
	if len(points) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, p := range points {
		sum += p.Value
	}
	return sum / float64(len(points))
}

// ApplyMaxOverTime returns the maximum value across all points.
func ApplyMaxOverTime(points []TimeValue) float64 {
	if len(points) == 0 {
		return math.NaN()
	}
	m := points[0].Value
	for _, p := range points[1:] {
		if p.Value > m {
			m = p.Value
		}
	}
	return m
}

// ApplyMinOverTime returns the minimum value across all points.
func ApplyMinOverTime(points []TimeValue) float64 {
	if len(points) == 0 {
		return math.NaN()
	}
	m := points[0].Value
	for _, p := range points[1:] {
		if p.Value < m {
			m = p.Value
		}
	}
	return m
}

// bucket is an internal type for histogram quantile computation.
type bucket struct {
	upperBound float64
	count      float64
}

// HistogramQuantile computes the q-quantile (0 <= q <= 1) from a set of
// histogram buckets. The input map keys are the upper-bound (le) values and
// the map values are cumulative counts. This implements standard Prometheus
// linear interpolation between bucket boundaries.
func HistogramQuantile(q float64, buckets map[float64]float64) float64 {
	if len(buckets) == 0 || math.IsNaN(q) {
		return math.NaN()
	}
	if q < 0 || q > 1 {
		return math.NaN()
	}

	// Sort buckets by upper bound.
	sorted := make([]bucket, 0, len(buckets))
	for le, count := range buckets {
		sorted = append(sorted, bucket{upperBound: le, count: count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].upperBound < sorted[j].upperBound
	})

	// The highest bucket must be +Inf for a well-formed histogram. If it is
	// not present, we still do our best with what we have.

	// Total observations is the count in the highest bucket.
	total := sorted[len(sorted)-1].count
	if total == 0 {
		return math.NaN()
	}

	// Target rank.
	rank := q * total

	// Find the bucket where the rank falls and interpolate.
	var prevCount float64
	var prevBound float64

	for _, b := range sorted {
		if b.count >= rank {
			// Linear interpolation within this bucket.
			bucketCount := b.count - prevCount
			if bucketCount == 0 {
				return b.upperBound
			}
			// How far into this bucket the rank falls.
			fraction := (rank - prevCount) / bucketCount

			// Interpolate between previous bound and this bound.
			return prevBound + fraction*(b.upperBound-prevBound)
		}
		prevCount = b.count
		prevBound = b.upperBound
	}

	// Should not be reached for well-formed histograms; return the highest
	// finite bound.
	return sorted[len(sorted)-1].upperBound
}

// AggregateBy applies an aggregation operation across grouped series values.
// Supported ops: "sum", "avg", "count". The input map keys are group labels
// and values are the collected float64 values for that group.
func AggregateBy(op string, seriesGroups map[string][]float64) map[string]float64 {
	result := make(map[string]float64, len(seriesGroups))
	for group, values := range seriesGroups {
		if len(values) == 0 {
			continue
		}
		switch op {
		case "sum":
			var s float64
			for _, v := range values {
				s += v
			}
			result[group] = s
		case "avg":
			var s float64
			for _, v := range values {
				s += v
			}
			result[group] = s / float64(len(values))
		case "count":
			result[group] = float64(len(values))
		}
	}
	return result
}
