package tsdb

import (
	"testing"
	"time"
)

func TestParseExpr_BareMetricName(t *testing.T) {
	expr, err := ParseExpr("vm_cpu_usage_percent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Type != ExprSelector {
		t.Fatalf("expected ExprSelector, got %d", expr.Type)
	}
	if expr.Selector.Name != "vm_cpu_usage_percent" {
		t.Fatalf("expected name vm_cpu_usage_percent, got %s", expr.Selector.Name)
	}
	if len(expr.Selector.Matchers) != 0 {
		t.Fatalf("expected 0 matchers, got %d", len(expr.Selector.Matchers))
	}
}

func TestParseExpr_SelectorWithLabels(t *testing.T) {
	expr, err := ParseExpr(`vm_cpu_usage_percent{type="service", region=~"us-.*"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Type != ExprSelector {
		t.Fatalf("expected ExprSelector, got %d", expr.Type)
	}
	sel := expr.Selector
	if sel.Name != "vm_cpu_usage_percent" {
		t.Fatalf("unexpected name: %s", sel.Name)
	}
	if len(sel.Matchers) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(sel.Matchers))
	}

	m0 := sel.Matchers[0]
	if m0.Name != "type" || m0.Value != "service" || m0.Type != MatchEqual {
		t.Fatalf("matcher[0] = %+v", m0)
	}
	m1 := sel.Matchers[1]
	if m1.Name != "region" || m1.Value != "us-.*" || m1.Type != MatchRegexp {
		t.Fatalf("matcher[1] = %+v", m1)
	}
}

func TestParseExpr_AllMatcherTypes(t *testing.T) {
	expr, err := ParseExpr(`m{a="1", b!="2", c=~"3", d!~"4"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	matchers := expr.Selector.Matchers
	if len(matchers) != 4 {
		t.Fatalf("expected 4 matchers, got %d", len(matchers))
	}
	expected := []MatchType{MatchEqual, MatchNotEqual, MatchRegexp, MatchNotRegexp}
	for i, mt := range expected {
		if matchers[i].Type != mt {
			t.Errorf("matcher[%d]: expected type %v, got %v", i, mt, matchers[i].Type)
		}
	}
}

func TestParseExpr_RateFunctionWithRange(t *testing.T) {
	expr, err := ParseExpr(`rate(gateway_requests_total{type="service"}[5m])`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Type != ExprFunction {
		t.Fatalf("expected ExprFunction, got %d", expr.Type)
	}
	fn := expr.Function
	if fn.Name != "rate" {
		t.Fatalf("expected function name rate, got %s", fn.Name)
	}
	if fn.Range != 5*time.Minute {
		t.Fatalf("expected 5m range, got %v", fn.Range)
	}
	if fn.Selector.Name != "gateway_requests_total" {
		t.Fatalf("expected metric name gateway_requests_total, got %s", fn.Selector.Name)
	}
	if len(fn.Selector.Matchers) != 1 || fn.Selector.Matchers[0].Value != "service" {
		t.Fatalf("unexpected matchers: %+v", fn.Selector.Matchers)
	}
}

func TestParseExpr_RangeFunctions(t *testing.T) {
	funcs := []string{"rate", "delta", "increase", "avg_over_time", "max_over_time", "min_over_time"}
	for _, name := range funcs {
		input := name + `(metric_name[1h])`
		expr, err := ParseExpr(input)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if expr.Type != ExprFunction {
			t.Fatalf("%s: expected ExprFunction", name)
		}
		if expr.Function.Name != name {
			t.Fatalf("%s: expected function name %s, got %s", name, name, expr.Function.Name)
		}
		if expr.Function.Range != time.Hour {
			t.Fatalf("%s: expected 1h range, got %v", name, expr.Function.Range)
		}
	}
}

func TestParseExpr_AggregationWithBy(t *testing.T) {
	expr, err := ParseExpr("sum(worker_active_sessions) by (worker_id)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Type != ExprAggregation {
		t.Fatalf("expected ExprAggregation, got %d", expr.Type)
	}
	agg := expr.Aggregation
	if agg.Op != "sum" {
		t.Fatalf("expected op sum, got %s", agg.Op)
	}
	if agg.Selector.Name != "worker_active_sessions" {
		t.Fatalf("unexpected metric: %s", agg.Selector.Name)
	}
	if len(agg.By) != 1 || agg.By[0] != "worker_id" {
		t.Fatalf("expected by [worker_id], got %v", agg.By)
	}
}

func TestParseExpr_AggregationWithoutBy(t *testing.T) {
	expr, err := ParseExpr("count(errors_total)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Type != ExprAggregation {
		t.Fatalf("expected ExprAggregation, got %d", expr.Type)
	}
	if expr.Aggregation.Op != "count" {
		t.Fatalf("expected count, got %s", expr.Aggregation.Op)
	}
	if len(expr.Aggregation.By) != 0 {
		t.Fatalf("expected empty by, got %v", expr.Aggregation.By)
	}
}

func TestParseExpr_AggregationMultipleByLabels(t *testing.T) {
	expr, err := ParseExpr("avg(cpu_usage{env=\"prod\"}) by (region, zone)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	agg := expr.Aggregation
	if agg.Op != "avg" {
		t.Fatalf("expected avg, got %s", agg.Op)
	}
	if len(agg.By) != 2 || agg.By[0] != "region" || agg.By[1] != "zone" {
		t.Fatalf("expected by [region zone], got %v", agg.By)
	}
	if len(agg.Selector.Matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(agg.Selector.Matchers))
	}
}

func TestParseExpr_HistogramQuantile(t *testing.T) {
	expr, err := ParseExpr(`histogram_quantile(0.95, vm_http_latency_seconds_bucket{type="service"})`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Type != ExprFunction {
		t.Fatalf("expected ExprFunction, got %d", expr.Type)
	}
	fn := expr.Function
	if fn.Name != "histogram_quantile" {
		t.Fatalf("expected histogram_quantile, got %s", fn.Name)
	}
	if len(fn.Args) != 1 || fn.Args[0] != 0.95 {
		t.Fatalf("expected args [0.95], got %v", fn.Args)
	}
	if fn.Selector.Name != "vm_http_latency_seconds_bucket" {
		t.Fatalf("unexpected metric: %s", fn.Selector.Name)
	}
	if len(fn.Selector.Matchers) != 1 {
		t.Fatalf("expected 1 matcher, got %d", len(fn.Selector.Matchers))
	}
}

func TestParseExpr_DurationVariants(t *testing.T) {
	cases := []struct {
		dur      string
		expected time.Duration
	}{
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		input := "rate(m[" + tc.dur + "])"
		expr, err := ParseExpr(input)
		if err != nil {
			t.Fatalf("duration %s: %v", tc.dur, err)
		}
		if expr.Function.Range != tc.expected {
			t.Errorf("duration %s: expected %v, got %v", tc.dur, tc.expected, expr.Function.Range)
		}
	}
}

func TestParseExpr_WhitespaceVariations(t *testing.T) {
	inputs := []string{
		`  rate( metric_name { label = "val" } [ 5m ] )  `,
		`rate(metric_name{label="val"}[5m])`,
	}
	for _, input := range inputs {
		expr, err := ParseExpr(input)
		if err != nil {
			t.Fatalf("input %q: %v", input, err)
		}
		if expr.Function.Name != "rate" {
			t.Errorf("input %q: expected rate, got %s", input, expr.Function.Name)
		}
		if expr.Function.Selector.Matchers[0].Value != "val" {
			t.Errorf("input %q: unexpected matcher value", input)
		}
	}
}

func TestParseExpr_Errors(t *testing.T) {
	cases := []string{
		"",
		"123invalid",
		`metric{label=unquoted}`,
		`rate(metric)`,           // missing range
		`unknown_func(metric)`,   // unknown function
		`metric{label="unterminated`,
		`sum(metric) by`,         // missing label list
	}
	for _, input := range cases {
		_, err := ParseExpr(input)
		if err == nil {
			t.Errorf("expected error for input %q, got nil", input)
		}
	}
}

func TestParseExpr_EmptyLabelMatchers(t *testing.T) {
	expr, err := ParseExpr("metric{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expr.Selector.Name != "metric" {
		t.Fatalf("unexpected name: %s", expr.Selector.Name)
	}
	if len(expr.Selector.Matchers) != 0 {
		t.Fatalf("expected 0 matchers, got %d", len(expr.Selector.Matchers))
	}
}

func TestParseExpr_EscapedStringValue(t *testing.T) {
	expr, err := ParseExpr(`metric{label="value\"with\\escape"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := expr.Selector.Matchers[0]
	if m.Value != `value"with\escape` {
		t.Fatalf("unexpected value: %q", m.Value)
	}
}
