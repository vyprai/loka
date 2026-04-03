package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/metrics/scraper"
	"github.com/vyprai/loka/internal/controlplane/metrics/tsdb"
)

// MetricsAPI provides Prometheus-compatible HTTP query handlers.
type MetricsAPI struct {
	Store   tsdb.MetricsStore
	Scraper *scraper.Scraper
}

// Routes returns a chi sub-router with all metrics API endpoints mounted.
func (api *MetricsAPI) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/query", api.handleInstantQuery)
	r.Get("/query_range", api.handleRangeQuery)
	r.Get("/series", api.handleSeries)
	r.Get("/labels", api.handleLabels)
	r.Get("/label/{name}/values", api.handleLabelValues)
	r.Get("/targets", api.handleTargets)
	return r
}

// promResponse is the top-level Prometheus API response envelope.
type promResponse struct {
	Status    string      `json:"status"`
	Data      interface{} `json:"data,omitempty"`
	ErrorType string      `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// vectorData is the data payload for an instant query response.
type vectorData struct {
	ResultType string         `json:"resultType"`
	Result     []vectorResult `json:"result"`
}

// vectorResult is a single instant-query result entry.
type vectorResult struct {
	Metric map[string]string `json:"metric"`
	Value  [2]interface{}    `json:"value"` // [unix_timestamp, "string_value"]
}

// matrixData is the data payload for a range query response.
type matrixData struct {
	ResultType string         `json:"resultType"`
	Result     []matrixResult `json:"result"`
}

// matrixResult is a single range-query result entry.
type matrixResult struct {
	Metric map[string]string `json:"metric"`
	Values [][2]interface{}  `json:"values"` // [[ts, "val"], ...]
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeSuccess(w http.ResponseWriter, data interface{}) {
	writeJSON(w, http.StatusOK, promResponse{
		Status: "success",
		Data:   data,
	})
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, promResponse{
		Status:    "error",
		ErrorType: errType,
		Error:     msg,
	})
}

// handleInstantQuery implements GET /query.
func (api *MetricsAPI) handleInstantQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing required parameter: query")
		return
	}

	evalTime := time.Now()
	if ts := r.URL.Query().Get("time"); ts != "" {
		t, err := parseTime(ts)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid time parameter: %v", err))
			return
		}
		evalTime = t
	}

	expr, err := tsdb.ParseExpr(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid query expression: %v", err))
		return
	}

	ctx := r.Context()

	results, err := api.evaluateInstant(ctx, expr, evalTime)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}

	writeSuccess(w, vectorData{
		ResultType: "vector",
		Result:     results,
	})
}

// handleRangeQuery implements GET /query_range.
func (api *MetricsAPI) handleRangeQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing required parameter: query")
		return
	}

	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	stepStr := r.URL.Query().Get("step")

	if startStr == "" || endStr == "" || stepStr == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing required parameters: start, end, step")
		return
	}

	start, err := parseTime(startStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid start: %v", err))
		return
	}
	end, err := parseTime(endStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid end: %v", err))
		return
	}
	step, err := parseDuration(stepStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid step: %v", err))
		return
	}

	expr, err := tsdb.ParseExpr(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid query expression: %v", err))
		return
	}

	ctx := r.Context()

	results, err := api.evaluateRange(ctx, expr, start, end, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}

	writeSuccess(w, matrixData{
		ResultType: "matrix",
		Result:     results,
	})
}

// handleSeries implements GET /series.
func (api *MetricsAPI) handleSeries(w http.ResponseWriter, r *http.Request) {
	matchParams := r.URL.Query()["match[]"]
	if len(matchParams) == 0 {
		writeError(w, http.StatusBadRequest, "bad_data", "missing required parameter: match[]")
		return
	}

	ctx := r.Context()
	seen := make(map[uint64]map[string]string)

	for _, m := range matchParams {
		expr, err := tsdb.ParseExpr(m)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid match expression: %v", err))
			return
		}

		sel := selectorFromExpr(expr)
		if sel == nil {
			writeError(w, http.StatusBadRequest, "bad_data", "match expression must be a selector")
			return
		}

		matchers := buildMatchers(sel)
		seriesIDs, err := api.Store.FindSeries(ctx, matchers)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}

		for _, sid := range seriesIDs {
			if _, ok := seen[sid]; ok {
				continue
			}
			info, err := api.Store.GetSeriesInfo(ctx, sid)
			if err != nil {
				continue
			}
			labels := make(map[string]string, len(info.Labels)+1)
			labels["__name__"] = info.Name
			for k, v := range info.Labels {
				labels[k] = v
			}
			seen[sid] = labels
		}
	}

	result := make([]map[string]string, 0, len(seen))
	for _, labels := range seen {
		result = append(result, labels)
	}

	writeSuccess(w, result)
}

// handleLabels implements GET /labels.
func (api *MetricsAPI) handleLabels(w http.ResponseWriter, r *http.Request) {
	names, err := api.Store.ListLabelNames(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	writeSuccess(w, names)
}

// handleLabelValues implements GET /label/{name}/values.
func (api *MetricsAPI) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	labelName := chi.URLParam(r, "name")
	if labelName == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing label name")
		return
	}

	var values []string
	var err error

	if labelName == "__name__" {
		values, err = api.Store.ListMetrics(r.Context())
	} else {
		values, err = api.Store.ListLabelValues(r.Context(), labelName)
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	if values == nil {
		values = []string{}
	}
	writeSuccess(w, values)
}

// handleTargets implements GET /targets.
func (api *MetricsAPI) handleTargets(w http.ResponseWriter, r *http.Request) {
	if api.Scraper == nil {
		writeSuccess(w, map[string]interface{}{
			"activeTargets":  []interface{}{},
			"droppedTargets": []interface{}{},
		})
		return
	}

	results := api.Scraper.Targets()
	active := make([]map[string]interface{}, 0, len(results))

	for _, sr := range results {
		health := "down"
		if sr.Up {
			health = "up"
		}
		lastErr := ""
		if sr.Error != "" {
			lastErr = sr.Error
		}
		target := map[string]interface{}{
			"discoveredLabels": sr.Target.Labels,
			"labels":           sr.Target.Labels,
			"scrapePool":       sr.Target.Type,
			"scrapeUrl":        fmt.Sprintf("http://%s/metrics", sr.Target.Address),
			"health":           health,
			"lastScrape":       sr.LastScrape.Format(time.RFC3339),
			"lastScrapeDuration": sr.Duration.Seconds(),
			"lastError":          lastErr,
		}
		active = append(active, target)
	}

	writeSuccess(w, map[string]interface{}{
		"activeTargets":  active,
		"droppedTargets": []interface{}{},
	})
}

// evaluateInstant evaluates an expression at a single point in time.
func (api *MetricsAPI) evaluateInstant(ctx context.Context, expr *tsdb.Expr, evalTime time.Time) ([]vectorResult, error) {
	switch expr.Type {
	case tsdb.ExprSelector:
		return api.evalSelectorInstant(ctx, expr.Selector, evalTime)

	case tsdb.ExprFunction:
		return api.evalFunctionInstant(ctx, expr.Function, evalTime)

	case tsdb.ExprAggregation:
		return api.evalAggregationInstant(ctx, expr.Aggregation, evalTime)

	default:
		return nil, fmt.Errorf("unsupported expression type")
	}
}

// evalSelectorInstant queries the latest value at evalTime for each matching series.
func (api *MetricsAPI) evalSelectorInstant(ctx context.Context, sel *tsdb.Selector, evalTime time.Time) ([]vectorResult, error) {
	matchers := buildMatchers(sel)
	seriesIDs, err := api.Store.FindSeries(ctx, matchers)
	if err != nil {
		return nil, err
	}

	// Look back 5 minutes to find the latest point.
	lookback := 5 * time.Minute
	qr, err := api.Store.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     evalTime.Add(-lookback),
		End:       evalTime,
	})
	if err != nil {
		return nil, err
	}

	var results []vectorResult
	for _, r := range qr {
		if len(r.Points) == 0 {
			continue
		}
		last := r.Points[len(r.Points)-1]
		info, err := api.Store.GetSeriesInfo(ctx, r.SeriesID)
		if err != nil {
			continue
		}
		metric := buildMetricLabels(info.Name, info.Labels)
		results = append(results, vectorResult{
			Metric: metric,
			Value:  formatValue(last),
		})
	}

	if results == nil {
		results = []vectorResult{}
	}
	return results, nil
}

// evalFunctionInstant evaluates a range function (rate, delta, etc.) at a single point.
func (api *MetricsAPI) evalFunctionInstant(ctx context.Context, fn *tsdb.FunctionCall, evalTime time.Time) ([]vectorResult, error) {
	matchers := buildMatchers(fn.Selector)
	seriesIDs, err := api.Store.FindSeries(ctx, matchers)
	if err != nil {
		return nil, err
	}

	rangeDur := fn.Range
	if rangeDur == 0 {
		rangeDur = 5 * time.Minute
	}

	qr, err := api.Store.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     evalTime.Add(-rangeDur),
		End:       evalTime,
	})
	if err != nil {
		return nil, err
	}

	var results []vectorResult
	for _, r := range qr {
		if len(r.Points) == 0 {
			continue
		}
		info, err := api.Store.GetSeriesInfo(ctx, r.SeriesID)
		if err != nil {
			continue
		}

		computed := applyFunction(fn.Name, r.Points, rangeDur)
		if len(computed) == 0 {
			continue
		}

		metric := buildMetricLabels(info.Name, info.Labels)
		results = append(results, vectorResult{
			Metric: metric,
			Value:  formatValue(computed[len(computed)-1]),
		})
	}

	if results == nil {
		results = []vectorResult{}
	}
	return results, nil
}

// evalAggregationInstant evaluates an aggregation at a single point in time.
func (api *MetricsAPI) evalAggregationInstant(ctx context.Context, agg *tsdb.AggregationExpr, evalTime time.Time) ([]vectorResult, error) {
	// First resolve the inner selector as an instant query.
	inner, err := api.evalSelectorInstant(ctx, agg.Selector, evalTime)
	if err != nil {
		return nil, err
	}

	// Group results by the "by" labels.
	type groupEntry struct {
		labels map[string]string
		values []float64
		ts     float64 // keep latest timestamp
	}
	groups := make(map[string]*groupEntry)

	for _, vr := range inner {
		groupKey := buildGroupKey(vr.Metric, agg.By)
		g, ok := groups[groupKey]
		if !ok {
			gl := make(map[string]string)
			for _, l := range agg.By {
				if v, exists := vr.Metric[l]; exists {
					gl[l] = v
				}
			}
			g = &groupEntry{labels: gl}
			groups[groupKey] = g
		}
		val, _ := strconv.ParseFloat(vr.Value[1].(string), 64)
		g.values = append(g.values, val)
		if vr.Value[0].(float64) > g.ts {
			g.ts = vr.Value[0].(float64)
		}
	}

	// Aggregate each group.
	collected := make(map[string][]float64, len(groups))
	for key, g := range groups {
		collected[key] = g.values
	}
	aggregated := tsdb.AggregateBy(agg.Op, collected)

	var results []vectorResult
	for key, val := range aggregated {
		g := groups[key]
		results = append(results, vectorResult{
			Metric: g.labels,
			Value:  [2]interface{}{g.ts, formatFloat(val)},
		})
	}

	if results == nil {
		results = []vectorResult{}
	}
	return results, nil
}

// evaluateRange evaluates an expression over a time range.
func (api *MetricsAPI) evaluateRange(ctx context.Context, expr *tsdb.Expr, start, end time.Time, step time.Duration) ([]matrixResult, error) {
	switch expr.Type {
	case tsdb.ExprSelector:
		return api.evalSelectorRange(ctx, expr.Selector, start, end, step)

	case tsdb.ExprFunction:
		return api.evalFunctionRange(ctx, expr.Function, start, end, step)

	case tsdb.ExprAggregation:
		return api.evalAggregationRange(ctx, expr.Aggregation, start, end, step)

	default:
		return nil, fmt.Errorf("unsupported expression type")
	}
}

// evalSelectorRange queries raw data over a time range.
func (api *MetricsAPI) evalSelectorRange(ctx context.Context, sel *tsdb.Selector, start, end time.Time, step time.Duration) ([]matrixResult, error) {
	matchers := buildMatchers(sel)
	seriesIDs, err := api.Store.FindSeries(ctx, matchers)
	if err != nil {
		return nil, err
	}

	qr, err := api.Store.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     start,
		End:       end,
		Step:      step,
	})
	if err != nil {
		return nil, err
	}

	var results []matrixResult
	for _, r := range qr {
		if len(r.Points) == 0 {
			continue
		}
		info, err := api.Store.GetSeriesInfo(ctx, r.SeriesID)
		if err != nil {
			continue
		}
		metric := buildMetricLabels(info.Name, info.Labels)
		values := make([][2]interface{}, len(r.Points))
		for i, p := range r.Points {
			values[i] = formatValue(p)
		}
		results = append(results, matrixResult{
			Metric: metric,
			Values: values,
		})
	}

	if results == nil {
		results = []matrixResult{}
	}
	return results, nil
}

// evalFunctionRange evaluates a range function at each step over the range.
func (api *MetricsAPI) evalFunctionRange(ctx context.Context, fn *tsdb.FunctionCall, start, end time.Time, step time.Duration) ([]matrixResult, error) {
	matchers := buildMatchers(fn.Selector)
	seriesIDs, err := api.Store.FindSeries(ctx, matchers)
	if err != nil {
		return nil, err
	}

	rangeDur := fn.Range
	if rangeDur == 0 {
		rangeDur = 5 * time.Minute
	}

	// Fetch data for the entire window including the lookback for the first step.
	qr, err := api.Store.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     start.Add(-rangeDur),
		End:       end,
	})
	if err != nil {
		return nil, err
	}

	var results []matrixResult
	for _, r := range qr {
		if len(r.Points) == 0 {
			continue
		}
		info, err := api.Store.GetSeriesInfo(ctx, r.SeriesID)
		if err != nil {
			continue
		}

		// Evaluate the function at each step.
		var values [][2]interface{}
		for t := start; !t.After(end); t = t.Add(step) {
			windowStart := t.Add(-rangeDur).UnixMilli()
			windowEnd := t.UnixMilli()

			// Collect points within the window.
			var windowPoints []tsdb.TimeValue
			for _, p := range r.Points {
				if p.TimestampMs >= windowStart && p.TimestampMs <= windowEnd {
					windowPoints = append(windowPoints, p)
				}
			}

			computed := applyFunction(fn.Name, windowPoints, rangeDur)
			if len(computed) > 0 {
				tv := tsdb.TimeValue{
					TimestampMs: t.UnixMilli(),
					Value:       computed[len(computed)-1].Value,
				}
				values = append(values, formatValue(tv))
			}
		}

		if len(values) > 0 {
			metric := buildMetricLabels(info.Name, info.Labels)
			results = append(results, matrixResult{
				Metric: metric,
				Values: values,
			})
		}
	}

	if results == nil {
		results = []matrixResult{}
	}
	return results, nil
}

// evalAggregationRange evaluates an aggregation over a time range.
func (api *MetricsAPI) evalAggregationRange(ctx context.Context, agg *tsdb.AggregationExpr, start, end time.Time, step time.Duration) ([]matrixResult, error) {
	// Get all raw range data for the inner selector.
	matchers := buildMatchers(agg.Selector)
	seriesIDs, err := api.Store.FindSeries(ctx, matchers)
	if err != nil {
		return nil, err
	}

	qr, err := api.Store.Query(ctx, tsdb.QueryRequest{
		SeriesIDs: seriesIDs,
		Start:     start,
		End:       end,
		Step:      step,
	})
	if err != nil {
		return nil, err
	}

	// Build series info map.
	type seriesData struct {
		info   map[string]string
		points []tsdb.TimeValue
	}
	seriesMap := make(map[uint64]*seriesData)
	for _, r := range qr {
		if len(r.Points) == 0 {
			continue
		}
		info, err := api.Store.GetSeriesInfo(ctx, r.SeriesID)
		if err != nil {
			continue
		}
		metric := buildMetricLabels(info.Name, info.Labels)
		seriesMap[r.SeriesID] = &seriesData{info: metric, points: r.Points}
	}

	// At each step, group and aggregate.
	type groupTimeSeries struct {
		labels map[string]string
		values [][2]interface{}
	}
	groupResults := make(map[string]*groupTimeSeries)

	for t := start; !t.After(end); t = t.Add(step) {
		tMs := t.UnixMilli()
		stepGroups := make(map[string][]float64)
		stepLabels := make(map[string]map[string]string)

		for _, sd := range seriesMap {
			// Find the closest point at or before this step.
			var val *float64
			for i := len(sd.points) - 1; i >= 0; i-- {
				if sd.points[i].TimestampMs <= tMs {
					v := sd.points[i].Value
					val = &v
					break
				}
			}
			if val == nil {
				continue
			}

			groupKey := buildGroupKey(sd.info, agg.By)
			stepGroups[groupKey] = append(stepGroups[groupKey], *val)
			if _, ok := stepLabels[groupKey]; !ok {
				gl := make(map[string]string)
				for _, l := range agg.By {
					if v, exists := sd.info[l]; exists {
						gl[l] = v
					}
				}
				stepLabels[groupKey] = gl
			}
		}

		aggregated := tsdb.AggregateBy(agg.Op, stepGroups)
		ts := float64(tMs) / 1000.0
		for key, val := range aggregated {
			gts, ok := groupResults[key]
			if !ok {
				gts = &groupTimeSeries{labels: stepLabels[key]}
				groupResults[key] = gts
			}
			gts.values = append(gts.values, [2]interface{}{ts, formatFloat(val)})
		}
	}

	var results []matrixResult
	for _, gts := range groupResults {
		results = append(results, matrixResult{
			Metric: gts.labels,
			Values: gts.values,
		})
	}

	if results == nil {
		results = []matrixResult{}
	}
	return results, nil
}

// --- helpers ---

// buildMatchers converts a parsed Selector into label matchers for the store.
func buildMatchers(sel *tsdb.Selector) []tsdb.LabelMatcher {
	matchers := make([]tsdb.LabelMatcher, 0, len(sel.Matchers)+1)
	if sel.Name != "" {
		matchers = append(matchers, tsdb.LabelMatcher{
			Name:  "__name__",
			Value: sel.Name,
			Type:  tsdb.MatchEqual,
		})
	}
	matchers = append(matchers, sel.Matchers...)
	return matchers
}

// selectorFromExpr extracts the selector from any expression type.
func selectorFromExpr(expr *tsdb.Expr) *tsdb.Selector {
	switch expr.Type {
	case tsdb.ExprSelector:
		return expr.Selector
	case tsdb.ExprFunction:
		if expr.Function != nil {
			return expr.Function.Selector
		}
	case tsdb.ExprAggregation:
		if expr.Aggregation != nil {
			return expr.Aggregation.Selector
		}
	}
	return nil
}

// buildMetricLabels creates the metric label map including __name__.
func buildMetricLabels(name string, labels map[string]string) map[string]string {
	m := make(map[string]string, len(labels)+1)
	m["__name__"] = name
	for k, v := range labels {
		m[k] = v
	}
	return m
}

// buildGroupKey creates a string key for aggregation grouping based on the "by" labels.
func buildGroupKey(metric map[string]string, by []string) string {
	if len(by) == 0 {
		return ""
	}
	parts := make([]string, len(by))
	for i, l := range by {
		parts[i] = l + "=" + metric[l]
	}
	return strings.Join(parts, ",")
}

// formatValue converts a TimeValue into the Prometheus [timestamp, "value"] format.
func formatValue(tv tsdb.TimeValue) [2]interface{} {
	return [2]interface{}{
		float64(tv.TimestampMs) / 1000.0,
		formatFloat(tv.Value),
	}
}

// formatFloat formats a float64 as a string, handling special values.
func formatFloat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	if math.IsInf(v, -1) {
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// applyFunction applies a range function to the given points.
func applyFunction(name string, points []tsdb.TimeValue, rangeDur time.Duration) []tsdb.TimeValue {
	switch name {
	case "rate":
		return tsdb.ApplyRate(points, rangeDur)
	case "delta":
		return tsdb.ApplyDelta(points, rangeDur)
	case "increase":
		return tsdb.ApplyIncrease(points, rangeDur)
	case "avg_over_time":
		avg := tsdb.ApplyAvgOverTime(points)
		if math.IsNaN(avg) {
			return nil
		}
		if len(points) == 0 {
			return nil
		}
		return []tsdb.TimeValue{{TimestampMs: points[len(points)-1].TimestampMs, Value: avg}}
	case "max_over_time":
		max := tsdb.ApplyMaxOverTime(points)
		if math.IsNaN(max) {
			return nil
		}
		if len(points) == 0 {
			return nil
		}
		return []tsdb.TimeValue{{TimestampMs: points[len(points)-1].TimestampMs, Value: max}}
	case "min_over_time":
		min := tsdb.ApplyMinOverTime(points)
		if math.IsNaN(min) {
			return nil
		}
		if len(points) == 0 {
			return nil
		}
		return []tsdb.TimeValue{{TimestampMs: points[len(points)-1].TimestampMs, Value: min}}
	default:
		return nil
	}
}

// parseTime parses a time string as either RFC3339 or a unix timestamp (float seconds).
func parseTime(s string) (time.Time, error) {
	// Try RFC3339 first.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Try RFC3339Nano.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	// Try unix timestamp (float seconds).
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q as RFC3339 or unix timestamp", s)
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec), nil
}

// parseDuration parses a Prometheus-style duration string: "15s", "1m", "5m", "1h", "24h", "7d".
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return 0, fmt.Errorf("empty duration")
	}

	// Try Go's time.ParseDuration first for simple cases like "15s", "1m", "1h".
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	// Handle "d" suffix (days) which Go doesn't support.
	var total time.Duration
	i := 0
	for i < len(s) {
		numStart := i
		for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
			i++
		}
		if i == numStart || i >= len(s) {
			return 0, fmt.Errorf("invalid duration: %q", s)
		}
		val, err := strconv.ParseFloat(s[numStart:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration number: %w", err)
		}
		unit := s[i]
		i++
		switch unit {
		case 's':
			total += time.Duration(val * float64(time.Second))
		case 'm':
			total += time.Duration(val * float64(time.Minute))
		case 'h':
			total += time.Duration(val * float64(time.Hour))
		case 'd':
			total += time.Duration(val * 24 * float64(time.Hour))
		case 'w':
			total += time.Duration(val * 7 * 24 * float64(time.Hour))
		default:
			return 0, fmt.Errorf("unknown duration unit: %q", string(unit))
		}
	}
	return total, nil
}
