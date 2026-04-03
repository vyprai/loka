package lokametrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metrics/query" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("query") != "up" {
			t.Errorf("unexpected query param: %s", r.URL.Query().Get("query"))
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		resp := QueryResponse{
			Status: "success",
			Data: ResultData{
				ResultType: "vector",
				Result:     json.RawMessage(`[{"metric":{"__name__":"up"},"value":[1609459200,"1"]}]`),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Query(context.Background(), "up", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected status success, got %s", resp.Status)
	}
	if resp.Data.ResultType != "vector" {
		t.Errorf("expected resultType vector, got %s", resp.Data.ResultType)
	}

	var results []VectorResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Metric["__name__"] != "up" {
		t.Errorf("unexpected metric name: %s", results[0].Metric["__name__"])
	}
	if results[0].Value.Float() != 1 {
		t.Errorf("expected value 1, got %f", results[0].Value.Float())
	}
}

func TestClientQueryRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metrics/query_range" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != "rate(http_requests_total[5m])" {
			t.Errorf("unexpected query: %s", q.Get("query"))
		}
		if q.Get("start") != "1609459200" {
			t.Errorf("unexpected start: %s", q.Get("start"))
		}
		if q.Get("end") != "1609462800" {
			t.Errorf("unexpected end: %s", q.Get("end"))
		}
		if q.Get("step") != "60" {
			t.Errorf("unexpected step: %s", q.Get("step"))
		}

		resp := QueryResponse{
			Status: "success",
			Data: ResultData{
				ResultType: "matrix",
				Result:     json.RawMessage(`[{"metric":{"__name__":"http_requests_total"},"values":[[1609459200,"10"],[1609459260,"20"]]}]`),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.QueryRange(context.Background(), "rate(http_requests_total[5m])", "1609459200", "1609462800", "60")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.ResultType != "matrix" {
		t.Errorf("expected matrix, got %s", resp.Data.ResultType)
	}

	var results []MatrixResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].Values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(results[0].Values))
	}
	if results[0].Values[1].Float() != 20 {
		t.Errorf("expected second value 20, got %f", results[0].Values[1].Float())
	}
}

func TestClientNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metrics/label/__name__/values" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		resp := NamesResponse{
			Status: "success",
			Data:   []string{"up", "http_requests_total", "cpu_usage"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	names, err := client.Names(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	expected := map[string]bool{"up": true, "http_requests_total": true, "cpu_usage": true}
	for _, n := range names {
		if !expected[n] {
			t.Errorf("unexpected name: %s", n)
		}
	}
}

func TestClientLabelValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metrics/label/job/values" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		resp := LabelValuesResponse{
			Status: "success",
			Data:   []string{"api-server", "worker", "scheduler"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	values, err := client.LabelValues(context.Background(), "job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 values, got %d", len(values))
	}
	expected := map[string]bool{"api-server": true, "worker": true, "scheduler": true}
	for _, v := range values {
		if !expected[v] {
			t.Errorf("unexpected value: %s", v)
		}
	}
}

func TestClientErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")

	_, err := client.Query(context.Background(), "up", nil)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}

	_, err = client.QueryRange(context.Background(), "up", "0", "1", "1")
	if err == nil {
		t.Fatal("expected error for 500 response on QueryRange, got nil")
	}

	_, err = client.Names(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response on Names, got nil")
	}

	_, err = client.LabelValues(context.Background(), "job")
	if err == nil {
		t.Fatal("expected error for 500 response on LabelValues, got nil")
	}
}
