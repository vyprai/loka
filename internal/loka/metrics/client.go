package lokametrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MetricsClient is a Prometheus-compatible HTTP API client for the Loka metrics endpoint.
type MetricsClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new metrics API client.
func NewClient(baseURL, token string) *MetricsClient {
	return &MetricsClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// --- Response types (Prometheus HTTP API compatible) ---

// QueryResponse is the top-level response from /query and /query_range.
type QueryResponse struct {
	Status    string     `json:"status"`
	Data      ResultData `json:"data"`
	ErrorType string     `json:"errorType,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// ResultData holds the query result type and values.
type ResultData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// VectorResult is a single instant-query result.
type VectorResult struct {
	Metric map[string]string `json:"metric"`
	Value  SamplePair        `json:"value"`
}

// MatrixResult is a single range-query result (time series).
type MatrixResult struct {
	Metric map[string]string `json:"metric"`
	Values []SamplePair      `json:"values"`
}

// SamplePair is a [timestamp, value] tuple.
type SamplePair [2]json.Number

// Timestamp returns the Unix timestamp.
func (sp SamplePair) Timestamp() float64 {
	f, _ := sp[0].Float64()
	return f
}

// Float returns the sample value as float64.
func (sp SamplePair) Float() float64 {
	f, _ := sp[1].Float64()
	return f
}

// NamesResponse holds metric name listing results.
type NamesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// LabelValuesResponse holds label value listing results.
type LabelValuesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// TargetsResponse holds scrape target status.
type TargetsResponse struct {
	Status string      `json:"status"`
	Data   TargetsData `json:"data"`
}

// TargetsData contains active and dropped targets.
type TargetsData struct {
	ActiveTargets  []Target `json:"activeTargets"`
	DroppedTargets []Target `json:"droppedTargets,omitempty"`
}

// Target represents a single scrape target.
type Target struct {
	Labels       map[string]string `json:"labels"`
	ScrapeURL    string            `json:"scrapeUrl"`
	Health       string            `json:"health"`
	LastScrape   string            `json:"lastScrape"`
	LastError    string            `json:"lastError,omitempty"`
	ScrapePool   string            `json:"scrapePool,omitempty"`
}

// --- API methods ---

// Query executes an instant PromQL query at an optional time.
func (c *MetricsClient) Query(ctx context.Context, query string, ts *time.Time) (*QueryResponse, error) {
	params := url.Values{"query": {query}}
	if ts != nil {
		params.Set("time", fmt.Sprintf("%d", ts.Unix()))
	}
	var resp QueryResponse
	if err := c.get(ctx, "/api/v1/metrics/query", params, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return &resp, fmt.Errorf("query error (%s): %s", resp.ErrorType, resp.Error)
	}
	return &resp, nil
}

// QueryRange executes a range PromQL query.
func (c *MetricsClient) QueryRange(ctx context.Context, query, start, end, step string) (*QueryResponse, error) {
	params := url.Values{
		"query": {query},
		"start": {start},
		"end":   {end},
		"step":  {step},
	}
	var resp QueryResponse
	if err := c.get(ctx, "/api/v1/metrics/query_range", params, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return &resp, fmt.Errorf("query_range error (%s): %s", resp.ErrorType, resp.Error)
	}
	return &resp, nil
}

// Names returns all known metric names.
func (c *MetricsClient) Names(ctx context.Context) ([]string, error) {
	var resp NamesResponse
	if err := c.get(ctx, "/api/v1/metrics/label/__name__/values", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// LabelValues returns all values for a given label name.
func (c *MetricsClient) LabelValues(ctx context.Context, labelName string) ([]string, error) {
	path := fmt.Sprintf("/api/v1/metrics/label/%s/values", url.PathEscape(labelName))
	var resp LabelValuesResponse
	if err := c.get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// Targets returns scrape target status.
func (c *MetricsClient) Targets(ctx context.Context) (*TargetsResponse, error) {
	var resp TargetsResponse
	if err := c.get(ctx, "/api/v1/metrics/targets", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- internal helpers ---

func (c *MetricsClient) get(ctx context.Context, path string, params url.Values, out interface{}) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("metrics request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metrics API returned %d: %s", resp.StatusCode, string(body))
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
