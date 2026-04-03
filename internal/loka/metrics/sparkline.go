package lokametrics

import "math"

// sparkBlocks are Unicode block elements from lowest to highest.
var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// RenderSparkline maps a slice of float64 values to a Unicode sparkline string.
// Width controls how many characters the output contains. If len(values) > width,
// values are bucketed by averaging; if fewer, they are spread across the width.
func RenderSparkline(values []float64, width int) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}

	// Bucket values to fit the target width.
	buckets := bucket(values, width)

	// Find min/max for scaling.
	minVal, maxVal := buckets[0], buckets[0]
	for _, v := range buckets {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	out := make([]rune, len(buckets))
	span := maxVal - minVal
	for i, v := range buckets {
		if span == 0 {
			out[i] = sparkBlocks[len(sparkBlocks)/2]
			continue
		}
		// Normalise to 0..1 then map to block index.
		norm := (v - minVal) / span
		idx := int(math.Round(norm * float64(len(sparkBlocks)-1)))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkBlocks) {
			idx = len(sparkBlocks) - 1
		}
		out[i] = sparkBlocks[idx]
	}
	return string(out)
}

// bucket averages values into n buckets.
func bucket(values []float64, n int) []float64 {
	if n >= len(values) {
		// Spread: duplicate values to fill width.
		out := make([]float64, n)
		for i := range out {
			srcIdx := i * len(values) / n
			if srcIdx >= len(values) {
				srcIdx = len(values) - 1
			}
			out[i] = values[srcIdx]
		}
		return out
	}

	out := make([]float64, n)
	bucketSize := float64(len(values)) / float64(n)
	for i := 0; i < n; i++ {
		start := int(float64(i) * bucketSize)
		end := int(float64(i+1) * bucketSize)
		if end > len(values) {
			end = len(values)
		}
		if start >= end {
			if i > 0 {
				out[i] = out[i-1]
			}
			continue
		}
		sum := 0.0
		for _, v := range values[start:end] {
			sum += v
		}
		out[i] = sum / float64(end-start)
	}
	return out
}
