package tsdb

import (
	"time"
)

// QueryRequest describes a time-series query.
type QueryRequest struct {
	// SeriesIDs is the set of series to query (resolved from label matchers).
	SeriesIDs []uint64

	// Start and End define the time range (inclusive).
	Start time.Time
	End   time.Time

	// Step is the aggregation interval. Zero means return raw points.
	Step time.Duration
}

// QueryResult holds the result for a single series.
type QueryResult struct {
	SeriesID uint64
	Points   []TimeValue
}

// TimeValue is a timestamp-value pair in Prometheus response format.
type TimeValue struct {
	TimestampMs int64
	Value       float64
}

// LabelMatcher matches a label value.
type LabelMatcher struct {
	Name  string
	Value string
	Type  MatchType
}

// MatchType determines how a label matcher operates.
type MatchType int

const (
	MatchEqual        MatchType = iota // =
	MatchNotEqual                      // !=
	MatchRegexp                        // =~
	MatchNotRegexp                     // !~
)

func (m MatchType) String() string {
	switch m {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	default:
		return "?"
	}
}
