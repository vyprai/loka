package alerting

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
)

// pendingState tracks how long a rule's condition has been continuously true.
type pendingState struct {
	since time.Time
}

// AlertManager evaluates alert rules on a fixed interval and fires/resolves
// alerts. It is safe for concurrent use.
type AlertManager struct {
	metrics tsdb.MetricsStore
	store   AlertRuleStore
	webhook *WebhookSender
	logger  *slog.Logger

	mu       sync.Mutex
	pending  map[string]*pendingState // ruleID -> pending state
	firing   map[string]*Alert        // ruleID -> currently-firing alert
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewAlertManager creates an AlertManager. Call Start to begin evaluation.
func NewAlertManager(metrics tsdb.MetricsStore, store AlertRuleStore, logger *slog.Logger) *AlertManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &AlertManager{
		metrics: metrics,
		store:   store,
		webhook: NewWebhookSender(logger),
		logger:  logger,
		pending: make(map[string]*pendingState),
		firing:  make(map[string]*Alert),
	}
}

// Start begins the background evaluation loop (every 15 seconds).
func (m *AlertManager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	m.wg.Add(1)
	go m.loop(ctx)
}

// Stop halts the background evaluation loop and waits for it to finish.
func (m *AlertManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

func (m *AlertManager) loop(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evaluate(ctx)
		}
	}
}

// evaluate loads all enabled rules and checks each one.
func (m *AlertManager) evaluate(ctx context.Context) {
	rules, err := m.store.ListAlertRules(ctx)
	if err != nil {
		m.logger.Error("failed to list alert rules", "error", err)
		return
	}

	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled {
			continue
		}
		if err := m.evaluateRule(ctx, rule); err != nil {
			m.logger.Warn("rule evaluation failed", "rule", rule.Name, "error", err)
		}
	}
}

// evaluateRule evaluates a single alert rule against the metrics store.
func (m *AlertManager) evaluateRule(ctx context.Context, rule *AlertRule) error {
	value, err := m.queryValue(ctx, rule.Query)
	if err != nil {
		return fmt.Errorf("query %q: %w", rule.Query, err)
	}

	conditionMet := checkCondition(value, rule.Condition, rule.Threshold)
	now := time.Now()

	forDuration, err := parseDuration(rule.For)
	if err != nil {
		return fmt.Errorf("parse for-duration %q: %w", rule.For, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if conditionMet {
		// Track pending state.
		ps, ok := m.pending[rule.ID]
		if !ok {
			ps = &pendingState{since: now}
			m.pending[rule.ID] = ps
		}

		// Has the condition been true long enough?
		if now.Sub(ps.since) >= forDuration {
			if _, alreadyFiring := m.firing[rule.ID]; !alreadyFiring {
				m.fire(ctx, rule, value, now)
			}
		}
	} else {
		// Condition no longer true — clear pending.
		delete(m.pending, rule.ID)

		// If the rule was firing, resolve.
		if alert, wasFiring := m.firing[rule.ID]; wasFiring {
			m.resolve(ctx, rule, alert, now)
		}
	}

	return nil
}

// fire creates a new alert and sends webhook notifications.
func (m *AlertManager) fire(ctx context.Context, rule *AlertRule, value float64, now time.Time) {
	alert := &Alert{
		ID:          uuid.New().String(),
		RuleID:      rule.ID,
		RuleName:    rule.Name,
		Status:      AlertFiring,
		Severity:    rule.Severity,
		Labels:      copyMap(rule.Labels),
		Annotations: copyMap(rule.Annotations),
		Value:       value,
		FiredAt:     now,
	}

	if err := m.store.CreateAlert(ctx, alert); err != nil {
		m.logger.Error("failed to persist alert", "rule", rule.Name, "error", err)
		return
	}

	m.firing[rule.ID] = alert
	m.logger.Info("alert firing", "rule", rule.Name, "severity", rule.Severity, "value", value)

	go m.webhook.Send(ctx, rule.Webhooks, AlertFiring, []Alert{*alert})
}

// resolve marks an existing alert as resolved and notifies webhooks.
func (m *AlertManager) resolve(ctx context.Context, rule *AlertRule, alert *Alert, now time.Time) {
	alert.Status = AlertResolved
	alert.ResolvedAt = &now

	if err := m.store.UpdateAlert(ctx, alert); err != nil {
		m.logger.Error("failed to update resolved alert", "rule", rule.Name, "error", err)
	}

	delete(m.firing, rule.ID)
	m.logger.Info("alert resolved", "rule", rule.Name)

	go m.webhook.Send(ctx, rule.Webhooks, AlertResolved, []Alert{*alert})
}

// DismissAlert marks an alert as dismissed by a user.
func (m *AlertManager) DismissAlert(ctx context.Context, alertID, dismissedBy string) error {
	if err := m.store.DismissAlert(ctx, alertID, dismissedBy); err != nil {
		return err
	}

	// Remove from firing map if present.
	m.mu.Lock()
	defer m.mu.Unlock()
	for ruleID, a := range m.firing {
		if a.ID == alertID {
			delete(m.firing, ruleID)
			delete(m.pending, ruleID)
			break
		}
	}

	return nil
}

// queryValue parses the rule's query expression, resolves matching series,
// and returns a single aggregate value (the latest data point averaged
// across matching series, or the result of a function/aggregation).
func (m *AlertManager) queryValue(ctx context.Context, query string) (float64, error) {
	expr, err := tsdb.ParseExpr(query)
	if err != nil {
		return 0, err
	}

	now := time.Now()

	switch expr.Type {
	case tsdb.ExprSelector:
		return m.evalSelector(ctx, expr.Selector, now)

	case tsdb.ExprFunction:
		return m.evalFunction(ctx, expr.Function, now)

	case tsdb.ExprAggregation:
		return m.evalAggregation(ctx, expr.Aggregation, now)

	default:
		return 0, fmt.Errorf("unsupported expression type")
	}
}

func (m *AlertManager) evalSelector(ctx context.Context, sel *tsdb.Selector, now time.Time) (float64, error) {
	matchers := buildMatchers(sel)
	seriesIDs, err := m.metrics.FindSeries(ctx, matchers)
	if err != nil {
		return 0, err
	}
	if len(seriesIDs) == 0 {
		return math.NaN(), nil
	}

	results, err := m.metrics.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     now.Add(-5 * time.Minute),
		End:       now,
	})
	if err != nil {
		return 0, err
	}

	return latestAvg(results), nil
}

func (m *AlertManager) evalFunction(ctx context.Context, fn *tsdb.FunctionCall, now time.Time) (float64, error) {
	matchers := buildMatchers(fn.Selector)
	seriesIDs, err := m.metrics.FindSeries(ctx, matchers)
	if err != nil {
		return 0, err
	}
	if len(seriesIDs) == 0 {
		return math.NaN(), nil
	}

	rangeDur := fn.Range
	if rangeDur == 0 {
		rangeDur = 5 * time.Minute
	}

	results, err := m.metrics.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     now.Add(-rangeDur),
		End:       now,
	})
	if err != nil {
		return 0, err
	}

	var total float64
	var count int
	for _, r := range results {
		if len(r.Points) == 0 {
			continue
		}
		var val float64
		switch fn.Name {
		case "rate":
			pts := tsdb.ApplyRate(r.Points, rangeDur)
			if len(pts) == 0 {
				continue
			}
			val = pts[0].Value
		case "delta":
			pts := tsdb.ApplyDelta(r.Points, rangeDur)
			if len(pts) == 0 {
				continue
			}
			val = pts[0].Value
		case "increase":
			pts := tsdb.ApplyIncrease(r.Points, rangeDur)
			if len(pts) == 0 {
				continue
			}
			val = pts[0].Value
		case "avg_over_time":
			val = tsdb.ApplyAvgOverTime(r.Points)
		case "max_over_time":
			val = tsdb.ApplyMaxOverTime(r.Points)
		case "min_over_time":
			val = tsdb.ApplyMinOverTime(r.Points)
		default:
			return 0, fmt.Errorf("unsupported function: %s", fn.Name)
		}
		total += val
		count++
	}

	if count == 0 {
		return math.NaN(), nil
	}
	return total / float64(count), nil
}

func (m *AlertManager) evalAggregation(ctx context.Context, agg *tsdb.AggregationExpr, now time.Time) (float64, error) {
	matchers := buildMatchers(agg.Selector)
	seriesIDs, err := m.metrics.FindSeries(ctx, matchers)
	if err != nil {
		return 0, err
	}
	if len(seriesIDs) == 0 {
		return math.NaN(), nil
	}

	results, err := m.metrics.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     now.Add(-5 * time.Minute),
		End:       now,
	})
	if err != nil {
		return 0, err
	}

	// Collect latest value from each series.
	var values []float64
	for _, r := range results {
		if len(r.Points) > 0 {
			values = append(values, r.Points[len(r.Points)-1].Value)
		}
	}
	if len(values) == 0 {
		return math.NaN(), nil
	}

	switch agg.Op {
	case "sum":
		var s float64
		for _, v := range values {
			s += v
		}
		return s, nil
	case "avg":
		var s float64
		for _, v := range values {
			s += v
		}
		return s / float64(len(values)), nil
	case "count":
		return float64(len(values)), nil
	default:
		return 0, fmt.Errorf("unsupported aggregation: %s", agg.Op)
	}
}

// buildMatchers converts a Selector into label matchers including __name__.
func buildMatchers(sel *tsdb.Selector) []tsdb.LabelMatcher {
	matchers := []tsdb.LabelMatcher{
		{Name: "__name__", Value: sel.Name, Type: tsdb.MatchEqual},
	}
	matchers = append(matchers, sel.Matchers...)
	return matchers
}

// latestAvg returns the average of the last point from each result series.
func latestAvg(results []tsdb.QueryResult) float64 {
	var sum float64
	var n int
	for _, r := range results {
		if len(r.Points) > 0 {
			sum += r.Points[len(r.Points)-1].Value
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return sum / float64(n)
}

// checkCondition evaluates: value <op> threshold.
func checkCondition(value float64, op string, threshold float64) bool {
	if math.IsNaN(value) {
		return false
	}
	switch op {
	case ">":
		return value > threshold
	case "<":
		return value < threshold
	case ">=":
		return value >= threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	case "!=":
		return value != threshold
	default:
		return false
	}
}

// parseDuration parses Prometheus-style duration strings (5m, 1h, 30s, etc.).
func parseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	// Try stdlib first for simple cases like "5m", "1h".
	d, err := time.ParseDuration(s)
	if err == nil {
		return d, nil
	}
	// Fall back for day/week units (e.g. "1d").
	return 0, fmt.Errorf("unsupported duration format: %s", s)
}

// copyMap returns a shallow copy of a string map, or nil if the input is nil.
func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
