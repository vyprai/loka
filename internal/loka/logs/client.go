package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// LogsClient is a Loki-compatible HTTP API client for the Loka logging endpoint.
type LogsClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new logs API client.
func NewClient(baseURL, token string) *LogsClient {
	return &LogsClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// --- Response types (Loki HTTP API compatible) ---

// QueryResponse is the top-level response from /query and /query_range.
type QueryResponse struct {
	Status string     `json:"status"`
	Data   ResultData `json:"data"`
	Error  string     `json:"error,omitempty"`
}

// ResultData holds the query result type and values.
type ResultData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// StreamResult is a single log stream with its entries.
type StreamResult struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"` // [["ts_ns", "line"], ...]
}

// --- API methods ---

// QueryRange executes a LogQL query over a time range.
func (c *LogsClient) QueryRange(ctx context.Context, query, start, end string, limit int) (*QueryResponse, error) {
	params := url.Values{
		"query": {query},
		"start": {start},
		"end":   {end},
		"limit": {strconv.Itoa(limit)},
	}
	var resp QueryResponse
	if err := c.get(ctx, "/api/v1/logs/query_range", params, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return &resp, fmt.Errorf("query_range error: %s", resp.Error)
	}
	return &resp, nil
}

// Query executes an instant LogQL query (latest entries).
func (c *LogsClient) Query(ctx context.Context, query string, limit int) (*QueryResponse, error) {
	params := url.Values{
		"query": {query},
		"limit": {strconv.Itoa(limit)},
	}
	var resp QueryResponse
	if err := c.get(ctx, "/api/v1/logs/query", params, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return &resp, fmt.Errorf("query error: %s", resp.Error)
	}
	return &resp, nil
}

// Tail polls for recent log entries (long-poll).
func (c *LogsClient) Tail(ctx context.Context, query string, limit int) (*QueryResponse, error) {
	params := url.Values{
		"query": {query},
		"limit": {strconv.Itoa(limit)},
	}
	var resp QueryResponse
	if err := c.get(ctx, "/api/v1/logs/tail", params, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return &resp, fmt.Errorf("tail error: %s", resp.Error)
	}
	return &resp, nil
}

// Labels returns all known label names.
func (c *LogsClient) Labels(ctx context.Context) ([]string, error) {
	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := c.get(ctx, "/api/v1/logs/labels", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// LabelValues returns all values for a given label name.
func (c *LogsClient) LabelValues(ctx context.Context, name string) ([]string, error) {
	path := fmt.Sprintf("/api/v1/logs/label/%s/values", url.PathEscape(name))
	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := c.get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// --- internal helpers ---

func (c *LogsClient) get(ctx context.Context, path string, params url.Values, out interface{}) error {
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
		return fmt.Errorf("logs request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("logs API returned %d: %s", resp.StatusCode, string(body))
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
