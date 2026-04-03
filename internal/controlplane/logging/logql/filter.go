package logql

import (
	"fmt"
	"regexp"
	"strings"
)

// CompileFilters compiles a list of LineFilter into a single predicate function.
// All filters are chained as AND: every filter must pass for the line to match.
// An empty filter list returns a function that accepts all lines.
func CompileFilters(filters []LineFilter) (func(string) bool, error) {
	if len(filters) == 0 {
		return func(string) bool { return true }, nil
	}

	predicates := make([]func(string) bool, 0, len(filters))
	for _, f := range filters {
		pred, err := compileFilter(f)
		if err != nil {
			return nil, err
		}
		predicates = append(predicates, pred)
	}

	return func(line string) bool {
		for _, pred := range predicates {
			if !pred(line) {
				return false
			}
		}
		return true
	}, nil
}

func compileFilter(f LineFilter) (func(string) bool, error) {
	switch f.Type {
	case FilterContains:
		pattern := f.Pattern
		return func(line string) bool {
			return strings.Contains(line, pattern)
		}, nil
	case FilterNotContains:
		pattern := f.Pattern
		return func(line string) bool {
			return !strings.Contains(line, pattern)
		}, nil
	case FilterRegex:
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex filter %q: %w", f.Pattern, err)
		}
		return func(line string) bool {
			return re.MatchString(line)
		}, nil
	case FilterNotRegex:
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex filter %q: %w", f.Pattern, err)
		}
		return func(line string) bool {
			return !re.MatchString(line)
		}, nil
	default:
		return nil, fmt.Errorf("unknown filter type: %d", f.Type)
	}
}
