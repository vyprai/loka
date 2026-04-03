package logql

// LogQuery is the parsed representation of a LogQL query.
type LogQuery struct {
	Matchers []LabelMatcher
	Filters  []LineFilter
}

// LabelMatcher matches against a label name/value pair.
type LabelMatcher struct {
	Name  string
	Value string
	Type  MatchType
}

// MatchType identifies the kind of label match operator.
type MatchType int

const (
	MatchEqual     MatchType = iota // =
	MatchNotEqual                   // !=
	MatchRegexp                     // =~
	MatchNotRegexp                  // !~
)

// LineFilter is a pipeline filter applied to log lines.
type LineFilter struct {
	Type    LineFilterType
	Pattern string
}

// LineFilterType identifies the kind of line filter operator.
type LineFilterType int

const (
	FilterContains    LineFilterType = iota // |=
	FilterNotContains                      // != (in pipeline context)
	FilterRegex                            // |~
	FilterNotRegex                         // !~
)
