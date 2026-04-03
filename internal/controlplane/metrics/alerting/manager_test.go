package alerting

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
	"github.com/vyprai/loka/internal/metrics"
)

// ---------------------------------------------------------------------------
// Mock AlertRuleStore
// ---------------------------------------------------------------------------

type mockAlertRuleStore struct {
	mu             sync.Mutex
	rules          map[string]*AlertRule
	alerts         map[string]*Alert
	recordingRules map[string]*RecordingRule
}

func newMockAlertRuleStore() *mockAlertRuleStore {
	return &mockAlertRuleStore{
		rules:          make(map[string]*AlertRule),
		alerts:         make(map[string]*Alert),
		recordingRules: make(map[string]*RecordingRule),
	}
}

func (s *mockAlertRuleStore) ListAlertRules(_ context.Context) ([]AlertRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AlertRule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, *r)
	}
	return out, nil
}

func (s *mockAlertRuleStore) GetAlertRule(_ context.Context, id string) (*AlertRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rules[id]
	if !ok {
		return nil, fmt.Errorf("rule %s not found", id)
	}
	cp := *r
	return &cp, nil
}

func (s *mockAlertRuleStore) CreateAlertRule(_ context.Context, rule *AlertRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[rule.ID] = rule
	return nil
}

func (s *mockAlertRuleStore) UpdateAlertRule(_ context.Context, rule *AlertRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[rule.ID] = rule
	return nil
}

func (s *mockAlertRuleStore) DeleteAlertRule(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rules, id)
	return nil
}

func (s *mockAlertRuleStore) ListAlerts(_ context.Context, status *AlertStatus, limit int) ([]Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alert, 0, len(s.alerts))
	for _, a := range s.alerts {
		if status != nil && a.Status != *status {
			continue
		}
		out = append(out, *a)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *mockAlertRuleStore) CreateAlert(_ context.Context, alert *Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *alert
	s.alerts[alert.ID] = &cp
	return nil
}

func (s *mockAlertRuleStore) UpdateAlert(_ context.Context, alert *Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *alert
	s.alerts[alert.ID] = &cp
	return nil
}

func (s *mockAlertRuleStore) DismissAlert(_ context.Context, id, dismissedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.alerts[id]
	if !ok {
		return fmt.Errorf("alert %s not found", id)
	}
	a.Status = AlertDismissed
	now := time.Now()
	a.DismissedAt = &now
	a.DismissedBy = dismissedBy
	return nil
}

func (s *mockAlertRuleStore) ListRecordingRules(_ context.Context) ([]RecordingRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordingRule, 0, len(s.recordingRules))
	for _, r := range s.recordingRules {
		out = append(out, *r)
	}
	return out, nil
}

func (s *mockAlertRuleStore) CreateRecordingRule(_ context.Context, rule *RecordingRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordingRules[rule.ID] = rule
	return nil
}

func (s *mockAlertRuleStore) DeleteRecordingRule(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recordingRules, id)
	return nil
}

// getAlerts is a test helper that returns a snapshot of stored alerts.
func (s *mockAlertRuleStore) getAlerts() []*Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Alert, 0, len(s.alerts))
	for _, a := range s.alerts {
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// ---------------------------------------------------------------------------
// Mock MetricsStore
// ---------------------------------------------------------------------------

type mockMetricsStore struct {
	mu     sync.Mutex
	series map[uint64][]tsdb.TimeValue // seriesID -> points
	labels map[uint64]map[string]string // seriesID -> labels (including __name__)
	stats  tsdb.Stats
}

func newMockMetricsStore() *mockMetricsStore {
	return &mockMetricsStore{
		series: make(map[uint64][]tsdb.TimeValue),
		labels: make(map[uint64]map[string]string),
	}
}

// addSeries adds a series with a metric name and optional labels,
// and writes data points for it.
func (m *mockMetricsStore) addSeries(name string, extraLabels map[string]string, points []tsdb.TimeValue) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Build labels the same way the real store would.
	ls := metrics.Labels{{Name: "__name__", Value: name}}
	allLabels := map[string]string{"__name__": name}
	for k, v := range extraLabels {
		ls = append(ls, metrics.Label{Name: k, Value: v})
		allLabels[k] = v
	}
	ls.Sort()
	sid := ls.Hash()

	m.series[sid] = append(m.series[sid], points...)
	m.labels[sid] = allLabels
	return sid
}

func (m *mockMetricsStore) Write(_ context.Context, _ []metrics.DataPoint) error {
	return nil // not used in manager tests
}

func (m *mockMetricsStore) Query(_ context.Context, req tsdb.QueryRequest) ([]tsdb.QueryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	startMs := req.Start.UnixMilli()
	endMs := req.End.UnixMilli()

	var results []tsdb.QueryResult
	for _, sid := range req.SeriesIDs {
		pts, ok := m.series[sid]
		if !ok {
			continue
		}
		var filtered []tsdb.TimeValue
		for _, p := range pts {
			if p.TimestampMs >= startMs && p.TimestampMs <= endMs {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) > 0 {
			results = append(results, tsdb.QueryResult{SeriesID: sid, Points: filtered})
		}
	}
	return results, nil
}

func (m *mockMetricsStore) ListMetrics(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]struct{}{}
	for _, lbls := range m.labels {
		if n, ok := lbls["__name__"]; ok {
			seen[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out, nil
}

func (m *mockMetricsStore) ListLabelNames(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockMetricsStore) ListLabelValues(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockMetricsStore) FindSeries(_ context.Context, matchers []tsdb.LabelMatcher) ([]uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []uint64
	for sid, lbls := range m.labels {
		matched := true
		for _, mat := range matchers {
			val, exists := lbls[mat.Name]
			switch mat.Type {
			case tsdb.MatchEqual:
				if !exists || val != mat.Value {
					matched = false
				}
			case tsdb.MatchNotEqual:
				if exists && val == mat.Value {
					matched = false
				}
			}
			if !matched {
				break
			}
		}
		if matched {
			result = append(result, sid)
		}
	}
	return result, nil
}

func (m *mockMetricsStore) GetSeriesInfo(_ context.Context, seriesID uint64) (*metrics.SeriesInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lbls, ok := m.labels[seriesID]
	if !ok {
		return nil, fmt.Errorf("series %d not found", seriesID)
	}
	return &metrics.SeriesInfo{
		Name:   lbls["__name__"],
		Labels: lbls,
	}, nil
}

func (m *mockMetricsStore) GetStats() *tsdb.Stats {
	return &m.stats
}

func (m *mockMetricsStore) DiskSize() (int64, int64) {
	return 0, 0
}

func (m *mockMetricsStore) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.Default()
}

// makeRule creates a simple alert rule for testing.
func makeRule(id, name, query, condition string, threshold float64, forDur string) *AlertRule {
	return &AlertRule{
		ID:        id,
		Name:      name,
		Query:     query,
		Condition: condition,
		Threshold: threshold,
		For:       forDur,
		Severity:  "warning",
		Enabled:   true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCheckCondition(t *testing.T) {
	tests := []struct {
		value     float64
		op        string
		threshold float64
		want      bool
	}{
		// Greater than
		{10, ">", 5, true},
		{5, ">", 5, false},
		{3, ">", 5, false},
		// Less than
		{3, "<", 5, true},
		{5, "<", 5, false},
		{10, "<", 5, false},
		// Greater than or equal
		{10, ">=", 5, true},
		{5, ">=", 5, true},
		{3, ">=", 5, false},
		// Less than or equal
		{3, "<=", 5, true},
		{5, "<=", 5, true},
		{10, "<=", 5, false},
		// Equal
		{5, "==", 5, true},
		{3, "==", 5, false},
		// Not equal
		{3, "!=", 5, true},
		{5, "!=", 5, false},
		// Unknown operator
		{5, "~", 5, false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%.1f %s %.1f", tt.value, tt.op, tt.threshold)
		t.Run(name, func(t *testing.T) {
			got := checkCondition(tt.value, tt.op, tt.threshold)
			if got != tt.want {
				t.Errorf("checkCondition(%v, %q, %v) = %v, want %v",
					tt.value, tt.op, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestAlertFiring(t *testing.T) {
	store := newMockAlertRuleStore()
	ms := newMockMetricsStore()
	ctx := context.Background()

	// Add a metric series with a value above the threshold.
	now := time.Now()
	ms.addSeries("cpu_usage", nil, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 95.0},
	})

	// Rule: cpu_usage > 80, no pending duration.
	rule := makeRule("r1", "HighCPU", "cpu_usage", ">", 80, "0")
	if err := store.CreateAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	mgr := NewAlertManager(ms, store, testLogger())

	// Evaluate once.
	mgr.evaluate(ctx)

	// Allow the async webhook goroutine to finish (it's a no-op here).
	time.Sleep(50 * time.Millisecond)

	alerts := store.getAlerts()
	if len(alerts) == 0 {
		t.Fatal("expected at least one alert, got none")
	}

	found := false
	for _, a := range alerts {
		if a.RuleID == "r1" && a.Status == AlertFiring {
			found = true
			if a.Value != 95.0 {
				t.Errorf("expected value 95.0, got %v", a.Value)
			}
		}
	}
	if !found {
		t.Error("expected a firing alert for rule r1")
	}
}

func TestAlertPendingDuration(t *testing.T) {
	store := newMockAlertRuleStore()
	ms := newMockMetricsStore()
	ctx := context.Background()

	now := time.Now()
	ms.addSeries("cpu_usage", nil, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 95.0},
	})

	// Rule: cpu_usage > 80, for 30s.
	rule := makeRule("r1", "HighCPU", "cpu_usage", ">", 80, "30s")
	if err := store.CreateAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	mgr := NewAlertManager(ms, store, testLogger())

	// First evaluation: condition is true, but pending duration not met yet.
	mgr.evaluate(ctx)
	time.Sleep(20 * time.Millisecond)

	alerts := store.getAlerts()
	if len(alerts) != 0 {
		t.Fatalf("expected no alerts yet (pending), got %d", len(alerts))
	}

	// The alert should be pending.
	mgr.mu.Lock()
	_, isPending := mgr.pending["r1"]
	mgr.mu.Unlock()
	if !isPending {
		t.Fatal("expected rule r1 to be in pending state")
	}

	// Simulate that the pending period has elapsed by backdating the pending state.
	mgr.mu.Lock()
	mgr.pending["r1"].since = time.Now().Add(-31 * time.Second)
	mgr.mu.Unlock()

	// Evaluate again: now the duration requirement is met.
	mgr.evaluate(ctx)
	time.Sleep(50 * time.Millisecond)

	alerts = store.getAlerts()
	if len(alerts) == 0 {
		t.Fatal("expected alert to fire after pending duration elapsed")
	}
	if alerts[0].Status != AlertFiring {
		t.Errorf("expected status %q, got %q", AlertFiring, alerts[0].Status)
	}
}

func TestAlertResolved(t *testing.T) {
	store := newMockAlertRuleStore()
	ms := newMockMetricsStore()
	ctx := context.Background()

	now := time.Now()
	ms.addSeries("cpu_usage", nil, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 95.0},
	})

	rule := makeRule("r1", "HighCPU", "cpu_usage", ">", 80, "0")
	if err := store.CreateAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	mgr := NewAlertManager(ms, store, testLogger())

	// Fire the alert.
	mgr.evaluate(ctx)
	time.Sleep(50 * time.Millisecond)

	alerts := store.getAlerts()
	if len(alerts) == 0 {
		t.Fatal("expected a firing alert")
	}

	// Now drop the metric value below threshold.
	ms.mu.Lock()
	for sid := range ms.series {
		ms.series[sid] = []tsdb.TimeValue{
			{TimestampMs: time.Now().UnixMilli(), Value: 50.0},
		}
	}
	ms.mu.Unlock()

	// Evaluate again: condition is now false, alert should resolve.
	mgr.evaluate(ctx)
	time.Sleep(50 * time.Millisecond)

	alerts = store.getAlerts()
	resolved := false
	for _, a := range alerts {
		if a.RuleID == "r1" && a.Status == AlertResolved {
			resolved = true
			if a.ResolvedAt == nil {
				t.Error("expected ResolvedAt to be set")
			}
		}
	}
	if !resolved {
		t.Error("expected alert to be resolved after value dropped below threshold")
	}
}

func TestDismissAlert(t *testing.T) {
	store := newMockAlertRuleStore()
	ms := newMockMetricsStore()
	ctx := context.Background()

	now := time.Now()
	ms.addSeries("cpu_usage", nil, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 95.0},
	})

	rule := makeRule("r1", "HighCPU", "cpu_usage", ">", 80, "0")
	if err := store.CreateAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}

	mgr := NewAlertManager(ms, store, testLogger())

	// Fire the alert.
	mgr.evaluate(ctx)
	time.Sleep(50 * time.Millisecond)

	alerts := store.getAlerts()
	if len(alerts) == 0 {
		t.Fatal("expected a firing alert")
	}

	alertID := alerts[0].ID

	// Dismiss the alert.
	if err := mgr.DismissAlert(ctx, alertID, "admin"); err != nil {
		t.Fatalf("DismissAlert failed: %v", err)
	}

	// Verify dismissed in store.
	alerts = store.getAlerts()
	found := false
	for _, a := range alerts {
		if a.ID == alertID {
			found = true
			if a.Status != AlertDismissed {
				t.Errorf("expected status %q, got %q", AlertDismissed, a.Status)
			}
			if a.DismissedBy != "admin" {
				t.Errorf("expected DismissedBy %q, got %q", "admin", a.DismissedBy)
			}
		}
	}
	if !found {
		t.Error("dismissed alert not found in store")
	}

	// Verify removed from firing map.
	mgr.mu.Lock()
	_, stillFiring := mgr.firing["r1"]
	mgr.mu.Unlock()
	if stillFiring {
		t.Error("expected rule r1 to be removed from firing map after dismiss")
	}
}

func TestMultipleRules(t *testing.T) {
	store := newMockAlertRuleStore()
	ms := newMockMetricsStore()
	ctx := context.Background()

	now := time.Now()

	// cpu_usage = 95 (above 80 threshold)
	ms.addSeries("cpu_usage", nil, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 95.0},
	})

	// mem_usage = 50 (below 80 threshold)
	ms.addSeries("mem_usage", nil, []tsdb.TimeValue{
		{TimestampMs: now.UnixMilli(), Value: 50.0},
	})

	rule1 := makeRule("r1", "HighCPU", "cpu_usage", ">", 80, "0")
	rule2 := makeRule("r2", "HighMem", "mem_usage", ">", 80, "0")
	if err := store.CreateAlertRule(ctx, rule1); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAlertRule(ctx, rule2); err != nil {
		t.Fatal(err)
	}

	mgr := NewAlertManager(ms, store, testLogger())

	mgr.evaluate(ctx)
	time.Sleep(50 * time.Millisecond)

	alerts := store.getAlerts()

	var cpuFired, memFired bool
	for _, a := range alerts {
		if a.RuleID == "r1" && a.Status == AlertFiring {
			cpuFired = true
		}
		if a.RuleID == "r2" && a.Status == AlertFiring {
			memFired = true
		}
	}

	if !cpuFired {
		t.Error("expected cpu_usage rule (r1) to fire")
	}
	if memFired {
		t.Error("expected mem_usage rule (r2) NOT to fire")
	}
}
