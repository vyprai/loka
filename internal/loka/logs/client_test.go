package logs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogsClientQueryRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/logs/query_range" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != `{app="web"}` {
			t.Errorf("unexpected query: %s", q.Get("query"))
		}
		if q.Get("start") != "1609459200" {
			t.Errorf("unexpected start: %s", q.Get("start"))
		}
		if q.Get("end") != "1609462800" {
			t.Errorf("unexpected end: %s", q.Get("end"))
		}
		if q.Get("limit") != "100" {
			t.Errorf("unexpected limit: %s", q.Get("limit"))
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		resp := QueryResponse{
			Status: "success",
			Data: ResultData{
				ResultType: "streams",
				Result:     json.RawMessage(`[{"stream":{"app":"web"},"values":[["1609459200000000000","line one"],["1609459201000000000","line two"]]}]`),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.QueryRange(context.Background(), `{app="web"}`, "1609459200", "1609462800", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected status success, got %s", resp.Status)
	}
	if resp.Data.ResultType != "streams" {
		t.Errorf("expected resultType streams, got %s", resp.Data.ResultType)
	}

	var results []StreamResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(results))
	}
	if results[0].Stream["app"] != "web" {
		t.Errorf("unexpected stream label: %v", results[0].Stream)
	}
	if len(results[0].Values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(results[0].Values))
	}
	if results[0].Values[0][1] != "line one" {
		t.Errorf("unexpected log line: %s", results[0].Values[0][1])
	}
}

func TestLogsClientQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/logs/query" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("query") != `{app="api"}` {
			t.Errorf("unexpected query: %s", r.URL.Query().Get("query"))
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("unexpected limit: %s", r.URL.Query().Get("limit"))
		}

		resp := QueryResponse{
			Status: "success",
			Data: ResultData{
				ResultType: "streams",
				Result:     json.RawMessage(`[{"stream":{"app":"api"},"values":[["1609459200000000000","hello"]]}]`),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Query(context.Background(), `{app="api"}`, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected success, got %s", resp.Status)
	}

	var results []StreamResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(results) != 1 || len(results[0].Values) != 1 {
		t.Fatalf("unexpected result shape: %d streams", len(results))
	}
	if results[0].Values[0][1] != "hello" {
		t.Errorf("unexpected line: %s", results[0].Values[0][1])
	}
}

func TestLogsClientLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/logs/labels" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		resp := struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}{
			Status: "success",
			Data:   []string{"app", "env", "level"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	labels, err := client.Labels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(labels) != 3 {
		t.Fatalf("expected 3 labels, got %d", len(labels))
	}
	expected := map[string]bool{"app": true, "env": true, "level": true}
	for _, l := range labels {
		if !expected[l] {
			t.Errorf("unexpected label: %s", l)
		}
	}
}

func TestLogsClientLabelValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/logs/label/app/values" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		resp := struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}{
			Status: "success",
			Data:   []string{"web", "api", "worker"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	values, err := client.LabelValues(context.Background(), "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 values, got %d", len(values))
	}
	expected := map[string]bool{"web": true, "api": true, "worker": true}
	for _, v := range values {
		if !expected[v] {
			t.Errorf("unexpected value: %s", v)
		}
	}
}

func TestLogsClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")

	_, err := client.QueryRange(context.Background(), `{app="web"}`, "0", "1", 10)
	if err == nil {
		t.Fatal("expected error for 500 response on QueryRange, got nil")
	}

	_, err = client.Query(context.Background(), `{app="web"}`, 10)
	if err == nil {
		t.Fatal("expected error for 500 response on Query, got nil")
	}

	_, err = client.Labels(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response on Labels, got nil")
	}

	_, err = client.LabelValues(context.Background(), "app")
	if err == nil {
		t.Fatal("expected error for 500 response on LabelValues, got nil")
	}
}
