package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCPClient_Post_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "temporary"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	client := NewCPClient(srv.URL, "token", nil, slog.Default())
	var result map[string]string
	err := client.post(context.Background(), "/test", nil, &result)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls (2 failures + 1 success), got %d", calls.Load())
	}
}

func TestCPClient_Post_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}))
	defer srv.Close()

	client := NewCPClient(srv.URL, "token", nil, slog.Default())
	err := client.post(context.Background(), "/test", nil, nil)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if calls.Load() != 1 {
		t.Errorf("should not retry 4xx, expected 1 call, got %d", calls.Load())
	}
}

func TestCPClient_Post_ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
	}))
	defer srv.Close()

	client := NewCPClient(srv.URL, "token", nil, slog.Default())
	err := client.post(context.Background(), "/test", nil, nil)
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if calls.Load() != 4 { // 1 initial + 3 retries
		t.Errorf("expected 4 calls, got %d", calls.Load())
	}
}

func TestCPClient_Post_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "fail"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Already cancelled.

	client := NewCPClient(srv.URL, "token", nil, slog.Default())
	err := client.post(ctx, "/test", nil, nil)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestCPClient_Post_ServerDown(t *testing.T) {
	client := NewCPClient("http://127.0.0.1:1", "token", nil, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.post(ctx, "/test", map[string]string{"key": "val"}, nil)
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

func TestCPClient_Post_RequestBody(t *testing.T) {
	var receivedBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	client := NewCPClient(srv.URL, "mytoken", nil, slog.Default())
	err := client.post(context.Background(), "/test", map[string]string{"hello": "world"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if receivedBody["hello"] != "world" {
		t.Errorf("expected body hello=world, got %v", receivedBody)
	}
}

func TestCPClient_Post_AuthHeader(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := NewCPClient(srv.URL, "secret-token", nil, slog.Default())
	client.post(context.Background(), "/test", nil, nil)
	if authHeader != "Bearer secret-token" {
		t.Errorf("expected Bearer token, got %q", authHeader)
	}
}

func TestCPClient_Post_RetryPreservesBody(t *testing.T) {
	var lastBody map[string]string
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		json.NewDecoder(r.Body).Decode(&lastBody)
		if n == 1 {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "temp"})
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := NewCPClient(srv.URL, "", nil, slog.Default())
	client.post(context.Background(), "/test", map[string]string{"data": "preserved"}, nil)

	// Body should be intact on the retry.
	if lastBody["data"] != "preserved" {
		t.Errorf("expected body preserved on retry, got %v", lastBody)
	}
}
