package logql

import (
	"testing"
)

func TestParseBareSelector(t *testing.T) {
	q, err := Parse(`{type="service"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(q.Matchers))
	}
	m := q.Matchers[0]
	if m.Name != "type" || m.Value != "service" || m.Type != MatchEqual {
		t.Errorf("unexpected matcher: %+v", m)
	}
	if len(q.Filters) != 0 {
		t.Errorf("expected 0 filters, got %d", len(q.Filters))
	}
}

func TestParseMultipleMatchers(t *testing.T) {
	q, err := Parse(`{type="service", id="svc_1"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Matchers) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(q.Matchers))
	}
	if q.Matchers[0].Name != "type" || q.Matchers[0].Value != "service" {
		t.Errorf("unexpected first matcher: %+v", q.Matchers[0])
	}
	if q.Matchers[1].Name != "id" || q.Matchers[1].Value != "svc_1" {
		t.Errorf("unexpected second matcher: %+v", q.Matchers[1])
	}
}

func TestParseAllMatchTypes(t *testing.T) {
	q, err := Parse(`{a="1", b!="2", c=~"3.*", d!~"4"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Matchers) != 4 {
		t.Fatalf("expected 4 matchers, got %d", len(q.Matchers))
	}
	expected := []struct {
		name  string
		value string
		mt    MatchType
	}{
		{"a", "1", MatchEqual},
		{"b", "2", MatchNotEqual},
		{"c", "3.*", MatchRegexp},
		{"d", "4", MatchNotRegexp},
	}
	for i, e := range expected {
		m := q.Matchers[i]
		if m.Name != e.name || m.Value != e.value || m.Type != e.mt {
			t.Errorf("matcher[%d]: expected %+v, got %+v", i, e, m)
		}
	}
}

func TestParseSelectorWithContainsFilter(t *testing.T) {
	q, err := Parse(`{type="service"} |= "error"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(q.Matchers))
	}
	if len(q.Filters) != 1 {
		t.Fatalf("expected 1 filter, got %d", len(q.Filters))
	}
	f := q.Filters[0]
	if f.Type != FilterContains || f.Pattern != "error" {
		t.Errorf("unexpected filter: %+v", f)
	}
}

func TestParseSelectorWithMultipleFilters(t *testing.T) {
	q, err := Parse(`{type="service"} |= "error" |~ "timeout"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Filters) != 2 {
		t.Fatalf("expected 2 filters, got %d", len(q.Filters))
	}
	if q.Filters[0].Type != FilterContains || q.Filters[0].Pattern != "error" {
		t.Errorf("unexpected filter[0]: %+v", q.Filters[0])
	}
	if q.Filters[1].Type != FilterRegex || q.Filters[1].Pattern != "timeout" {
		t.Errorf("unexpected filter[1]: %+v", q.Filters[1])
	}
}

func TestParseWithNotContains(t *testing.T) {
	q, err := Parse(`{type="service"} |= "error" != "health"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Filters) != 2 {
		t.Fatalf("expected 2 filters, got %d", len(q.Filters))
	}
	if q.Filters[0].Type != FilterContains || q.Filters[0].Pattern != "error" {
		t.Errorf("unexpected filter[0]: %+v", q.Filters[0])
	}
	if q.Filters[1].Type != FilterNotContains || q.Filters[1].Pattern != "health" {
		t.Errorf("unexpected filter[1]: %+v", q.Filters[1])
	}
}

func TestParseWithNotRegex(t *testing.T) {
	q, err := Parse(`{type="service"} !~ "debug"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.Filters) != 1 {
		t.Fatalf("expected 1 filter, got %d", len(q.Filters))
	}
	if q.Filters[0].Type != FilterNotRegex || q.Filters[0].Pattern != "debug" {
		t.Errorf("unexpected filter: %+v", q.Filters[0])
	}
}

func TestParseErrorEmptyInput(t *testing.T) {
	_, err := Parse("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseErrorMissingOpenBrace(t *testing.T) {
	_, err := Parse(`type="service"}`)
	if err == nil {
		t.Fatal("expected error for missing opening brace")
	}
}

func TestParseErrorUnterminatedString(t *testing.T) {
	_, err := Parse(`{type="service`)
	if err == nil {
		t.Fatal("expected error for unterminated string")
	}
}
