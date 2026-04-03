package metrics

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// MetricType represents the type of a metric.
type MetricType int

const (
	Gauge     MetricType = iota // Gauge is a metric that can go up and down.
	Counter                     // Counter is a metric that only increases.
	Histogram                   // Histogram tracks distributions of observations.
)

func (t MetricType) String() string {
	switch t {
	case Gauge:
		return "gauge"
	case Counter:
		return "counter"
	case Histogram:
		return "histogram"
	default:
		return "unknown"
	}
}

// ParseMetricType parses a metric type from its string representation.
func ParseMetricType(s string) (MetricType, error) {
	switch strings.ToLower(s) {
	case "gauge":
		return Gauge, nil
	case "counter":
		return Counter, nil
	case "histogram":
		return Histogram, nil
	default:
		return 0, fmt.Errorf("unknown metric type: %q", s)
	}
}

// Label is a key-value pair attached to a metric.
type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Labels is a sorted slice of Label.
type Labels []Label

func (ls Labels) Len() int           { return len(ls) }
func (ls Labels) Less(i, j int) bool { return ls[i].Name < ls[j].Name }
func (ls Labels) Swap(i, j int)      { ls[i], ls[j] = ls[j], ls[i] }

// Sort sorts labels by name in place.
func (ls Labels) Sort() { sort.Sort(ls) }

// Get returns the value for a label name, or empty string if not found.
func (ls Labels) Get(name string) string {
	for _, l := range ls {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

// Map returns labels as a map.
func (ls Labels) Map() map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

// LabelsFromMap creates a sorted Labels slice from a map.
func LabelsFromMap(m map[string]string) Labels {
	ls := make(Labels, 0, len(m))
	for k, v := range m {
		ls = append(ls, Label{Name: k, Value: v})
	}
	ls.Sort()
	return ls
}

// Canonical returns the canonical string representation of labels: "k1=v1,k2=v2,..."
func (ls Labels) Canonical() string {
	sorted := make(Labels, len(ls))
	copy(sorted, ls)
	sorted.Sort()

	var b strings.Builder
	for i, l := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(l.Value)
	}
	return b.String()
}

// Hash returns a FNV-1a hash of the canonical label representation.
func (ls Labels) Hash() uint64 {
	h := fnv.New64a()
	h.Write([]byte(ls.Canonical()))
	return h.Sum64()
}

// DataPoint is a single metric observation.
type DataPoint struct {
	Name      string     `json:"name"`
	Type      MetricType `json:"type"`
	Labels    Labels     `json:"labels"`
	Timestamp int64      `json:"ts"`    // Unix milliseconds.
	Value     float64    `json:"value"`
}

// SeriesID returns a unique identifier for this metric name + label combination.
func (dp DataPoint) SeriesID() uint64 {
	h := fnv.New64a()
	h.Write([]byte(dp.Name))
	h.Write([]byte{0})
	h.Write([]byte(dp.Labels.Canonical()))
	return h.Sum64()
}

// SeriesKey returns the canonical string key for this series: "name{labels}".
func (dp DataPoint) SeriesKey() string {
	c := dp.Labels.Canonical()
	if c == "" {
		return dp.Name
	}
	return dp.Name + "{" + c + "}"
}

// AggregatedValue stores pre-aggregated data for downsampled points.
type AggregatedValue struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
	Count int64   `json:"count"`
}

// SeriesInfo holds metadata about a time series.
type SeriesInfo struct {
	Name   string            `json:"name"`
	Type   MetricType        `json:"type"`
	Labels map[string]string `json:"labels"`
}
