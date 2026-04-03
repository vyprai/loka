package logql

import (
	"testing"
)

func TestContainsFilterMatches(t *testing.T) {
	fn, err := CompileFilters([]LineFilter{
		{Type: FilterContains, Pattern: "error"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fn("an error occurred") {
		t.Error("expected match for line containing 'error'")
	}
}

func TestContainsFilterNoMatch(t *testing.T) {
	fn, err := CompileFilters([]LineFilter{
		{Type: FilterContains, Pattern: "error"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fn("all systems nominal") {
		t.Error("expected no match for line without 'error'")
	}
}

func TestNotContainsFilter(t *testing.T) {
	fn, err := CompileFilters([]LineFilter{
		{Type: FilterNotContains, Pattern: "debug"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fn("an error occurred") {
		t.Error("expected match for line not containing 'debug'")
	}
	if fn("debug: starting up") {
		t.Error("expected no match for line containing 'debug'")
	}
}

func TestRegexFilter(t *testing.T) {
	fn, err := CompileFilters([]LineFilter{
		{Type: FilterRegex, Pattern: "timeout.*5s"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fn("connection timeout after 5s") {
		t.Error("expected match for regex pattern")
	}
	if fn("connection timeout after 10s") {
		t.Error("expected no match for non-matching line")
	}
}

func TestNotRegexFilter(t *testing.T) {
	fn, err := CompileFilters([]LineFilter{
		{Type: FilterNotRegex, Pattern: "^DEBUG"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fn("ERROR: something failed") {
		t.Error("expected match for line not matching regex")
	}
	if fn("DEBUG: trace info") {
		t.Error("expected no match for line matching regex")
	}
}

func TestMultipleFiltersChained(t *testing.T) {
	fn, err := CompileFilters([]LineFilter{
		{Type: FilterContains, Pattern: "error"},
		{Type: FilterNotContains, Pattern: "health"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fn("an error occurred in processing") {
		t.Error("expected match: contains 'error', no 'health'")
	}
	if fn("health check error") {
		t.Error("expected no match: contains both 'error' and 'health'")
	}
	if fn("all systems nominal") {
		t.Error("expected no match: doesn't contain 'error'")
	}
}

func TestEmptyFiltersPassEverything(t *testing.T) {
	fn, err := CompileFilters(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fn("anything at all") {
		t.Error("expected empty filters to pass everything")
	}
	if !fn("") {
		t.Error("expected empty filters to pass empty string")
	}
}
