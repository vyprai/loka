package logapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/logging/logql"
	"github.com/vyprai/loka/internal/controlplane/logging/store"
)

// LogsAPI provides Loki-compatible HTTP query handlers.
type LogsAPI struct {
	Store store.LogStore
}

// Routes returns a chi sub-router with all logging API endpoints mounted.
func (api *LogsAPI) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/query_range", api.handleQueryRange)
	r.Get("/query", api.handleQuery)
	r.Get("/labels", api.handleLabels)
	r.Get("/label/{name}/values", api.handleLabelValues)
	r.Get("/series", api.handleSeries)
	r.Get("/tail", api.handleTail)
	return r
}

// --- Loki-compatible response types ---

type lokiResponse struct {
	Status string      `json:"status"`
	Data   interface{} `json:"data,omitempty"`
	Error  string      `json:"error,omitempty"`
}

type streamsData struct {
	ResultType string         `json:"resultType"`
	Result     []streamResult `json:"result"`
}

type streamResult struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"` // [["ts_ns", "line"], ...]
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeSuccess(w http.ResponseWriter, data interface{}) {
	writeJSON(w, http.StatusOK, lokiResponse{
		Status: "success",
		Data:   data,
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, lokiResponse{
		Status: "error",
		Error:  msg,
	})
}

// handleQueryRange implements GET /query_range.
func (api *LogsAPI) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing required parameter: query")
		return
	}

	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	start := time.Now().Add(-time.Hour)
	end := time.Now()

	if startStr != "" {
		t, err := parseTime(startStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid start: %v", err))
			return
		}
		start = t
	}
	if endStr != "" {
		t, err := parseTime(endStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid end: %v", err))
			return
		}
		end = t
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	direction := r.URL.Query().Get("direction")
	if direction == "" {
		direction = "backward"
	}

	streams, err := api.executeQuery(r, query, start, end, limit, direction)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeSuccess(w, streamsData{
		ResultType: "streams",
		Result:     streams,
	})
}

// handleQuery implements GET /query (latest entries).
func (api *LogsAPI) handleQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing required parameter: query")
		return
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	end := time.Now()
	start := end.Add(-time.Hour)

	streams, err := api.executeQuery(r, query, start, end, limit, "backward")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeSuccess(w, streamsData{
		ResultType: "streams",
		Result:     streams,
	})
}

// handleLabels implements GET /labels.
func (api *LogsAPI) handleLabels(w http.ResponseWriter, r *http.Request) {
	names, err := api.Store.ListLabels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	writeSuccess(w, names)
}

// handleLabelValues implements GET /label/{name}/values.
func (api *LogsAPI) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	labelName := chi.URLParam(r, "name")
	if labelName == "" {
		writeError(w, http.StatusBadRequest, "missing label name")
		return
	}

	values, err := api.Store.ListLabelValues(r.Context(), labelName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if values == nil {
		values = []string{}
	}
	writeSuccess(w, values)
}

// handleSeries implements GET /series.
func (api *LogsAPI) handleSeries(w http.ResponseWriter, r *http.Request) {
	matchParams := r.URL.Query()["match[]"]
	if len(matchParams) == 0 {
		writeError(w, http.StatusBadRequest, "missing required parameter: match[]")
		return
	}

	ctx := r.Context()
	seen := make(map[uint64]map[string]string)

	for _, m := range matchParams {
		parsed, err := logql.Parse(m)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid match expression: %v", err))
			return
		}

		matchers := convertMatchers(parsed.Matchers)
		streamIDs, err := api.Store.FindStreams(ctx, matchers)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		for _, sid := range streamIDs {
			if _, ok := seen[sid]; ok {
				continue
			}
			labels, err := api.Store.GetStreamLabels(ctx, sid)
			if err != nil {
				continue
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

// handleTail implements GET /tail using long-poll.
func (api *LogsAPI) handleTail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing required parameter: query")
		return
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	// Long-poll: query the last 5 seconds of entries.
	end := time.Now()
	start := end.Add(-5 * time.Second)

	streams, err := api.executeQuery(r, query, start, end, limit, "forward")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeSuccess(w, streamsData{
		ResultType: "streams",
		Result:     streams,
	})
}

// executeQuery parses a LogQL query, resolves streams, queries entries, applies filters,
// and returns Loki-format stream results.
func (api *LogsAPI) executeQuery(r *http.Request, query string, start, end time.Time, limit int, direction string) ([]streamResult, error) {
	parsed, err := logql.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("invalid query: %w", err)
	}

	ctx := r.Context()

	// Convert logql matchers to store matchers.
	matchers := convertMatchers(parsed.Matchers)

	// Find matching streams.
	streamIDs, err := api.Store.FindStreams(ctx, matchers)
	if err != nil {
		return nil, err
	}

	if len(streamIDs) == 0 {
		return []streamResult{}, nil
	}

	// Compile line filters.
	lineFilter, err := logql.CompileFilters(parsed.Filters)
	if err != nil {
		return nil, fmt.Errorf("invalid filter: %w", err)
	}

	// Query entries with matchers and filters.
	result, err := api.Store.QueryWithMatchers(ctx, streamIDs, start, end, limit, direction, lineFilter)
	if err != nil {
		return nil, err
	}

	// Convert to Loki stream format.
	streams := make([]streamResult, 0, len(result.Streams))
	for _, s := range result.Streams {
		values := make([][2]string, 0, len(s.Entries))
		for _, e := range s.Entries {
			tsNs := strconv.FormatInt(e.Timestamp.UnixNano(), 10)
			values = append(values, [2]string{tsNs, e.Message})
		}
		streams = append(streams, streamResult{
			Stream: s.Labels,
			Values: values,
		})
	}

	if streams == nil {
		streams = []streamResult{}
	}
	return streams, nil
}

// convertMatchers converts logql.LabelMatcher to store.LabelMatcher.
func convertMatchers(lm []logql.LabelMatcher) []store.LabelMatcher {
	matchers := make([]store.LabelMatcher, len(lm))
	for i, m := range lm {
		var mt store.MatchType
		switch m.Type {
		case logql.MatchEqual:
			mt = store.MatchEqual
		case logql.MatchNotEqual:
			mt = store.MatchNotEqual
		case logql.MatchRegexp:
			mt = store.MatchRegexp
		case logql.MatchNotRegexp:
			mt = store.MatchNotRegexp
		}
		matchers[i] = store.LabelMatcher{
			Name:  m.Name,
			Value: m.Value,
			Type:  mt,
		}
	}
	return matchers
}

// parseTime parses a time string as RFC3339, RFC3339Nano, or unix timestamp (float seconds or nanoseconds).
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q as RFC3339 or unix timestamp", s)
	}
	// If value is very large, treat as nanoseconds (Loki sends ns).
	if f > 1e15 {
		ns := int64(f)
		return time.Unix(0, ns), nil
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec), nil
}
