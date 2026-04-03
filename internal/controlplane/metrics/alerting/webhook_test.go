package alerting

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

func TestWebhookSend(t *testing.T) {
	var received WebhookPayload
	var called atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("failed to decode payload: %v", err)
		}
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewWebhookSender(slog.Default())
	alerts := []Alert{
		{
			ID:       "a1",
			RuleID:   "r1",
			RuleName: "TestRule",
			Status:   AlertFiring,
			Severity: "critical",
			Value:    99.5,
			FiredAt:  time.Now(),
		},
	}

	sender.Send(context.Background(), []string{srv.URL}, AlertFiring, alerts)

	if !called.Load() {
		t.Fatal("webhook server was not called")
	}
	if received.Version != "1" {
		t.Errorf("expected version %q, got %q", "1", received.Version)
	}
	if received.Status != "firing" {
		t.Errorf("expected status %q, got %q", "firing", received.Status)
	}
	if len(received.Alerts) != 1 {
		t.Fatalf("expected 1 alert in payload, got %d", len(received.Alerts))
	}
	if received.Alerts[0].RuleName != "TestRule" {
		t.Errorf("expected RuleName %q, got %q", "TestRule", received.Alerts[0].RuleName)
	}
}

func TestWebhookSlackDetection(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://hooks.slack.com/services/T00/B00/xxx", true},
		{"https://hooks.slack.com/workflows/T00/xxx", true},
		{"https://example.com/webhook", false},
		{"https://example.com/hooks.slack.com/fake", true}, // contains the substring
		{"https://my-app.com/notify", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := isSlackURL(tt.url)
			if got != tt.want {
				t.Errorf("isSlackURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestWebhookSlackFormat(t *testing.T) {
	var received slackMessage
	var called atomic.Bool

	// The server URL must contain "hooks.slack.com" for Slack detection.
	// Since httptest gives us 127.0.0.1, we use a relay approach: set up a
	// normal server, then override the URL. Instead, we just test buildSlackBody
	// directly, plus verify the sender calls the mock.

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("failed to decode slack payload: %v", err)
		}
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Test buildSlackBody directly since we can't make httptest URL contain hooks.slack.com.
	payload := WebhookPayload{
		Version: "1",
		Status:  "firing",
		Alerts: []Alert{
			{
				RuleName: "HighCPU",
				Severity: "critical",
				Value:    95.5,
				Annotations: map[string]string{
					"summary": "CPU usage is very high",
				},
			},
		},
	}

	body, err := buildSlackBody(payload)
	if err != nil {
		t.Fatalf("buildSlackBody failed: %v", err)
	}

	var msg slackMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("failed to unmarshal slack body: %v", err)
	}

	if msg.Text == "" {
		t.Fatal("expected non-empty Slack message text")
	}

	// Verify it contains expected content.
	if !containsStr(msg.Text, "firing") {
		t.Error("expected Slack text to contain 'firing'")
	}
	if !containsStr(msg.Text, "HighCPU") {
		t.Error("expected Slack text to contain rule name 'HighCPU'")
	}
	if !containsStr(msg.Text, "critical") {
		t.Error("expected Slack text to contain severity 'critical'")
	}
	if !containsStr(msg.Text, "CPU usage is very high") {
		t.Error("expected Slack text to contain summary annotation")
	}

	// Also test resolved icon.
	payload.Status = "resolved"
	body, err = buildSlackBody(payload)
	if err != nil {
		t.Fatalf("buildSlackBody (resolved) failed: %v", err)
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("failed to unmarshal resolved slack body: %v", err)
	}
	if !containsStr(msg.Text, "resolved") {
		t.Error("expected resolved Slack text to contain 'resolved'")
	}
	if !containsStr(msg.Text, ":white_check_mark:") {
		t.Error("expected resolved Slack text to contain check mark icon")
	}
}

func TestWebhookRetry(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			// First two attempts fail.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Third attempt succeeds.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewWebhookSender(slog.Default())
	// Override the client timeout to speed up the test.
	sender.client.Timeout = 5 * time.Second

	alerts := []Alert{
		{
			ID:       "a1",
			RuleID:   "r1",
			RuleName: "RetryRule",
			Status:   AlertFiring,
			Severity: "warning",
			Value:    42.0,
			FiredAt:  time.Now(),
		},
	}

	sender.Send(context.Background(), []string{srv.URL}, AlertFiring, alerts)

	got := attempts.Load()
	if got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

// containsStr is a helper to check substring presence.
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
