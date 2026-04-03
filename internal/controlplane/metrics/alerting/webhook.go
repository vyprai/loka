package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// WebhookPayload is the JSON body sent to webhook endpoints.
type WebhookPayload struct {
	Version string  `json:"version"`
	Status  string  `json:"status"` // "firing" or "resolved"
	Alerts  []Alert `json:"alerts"`
}

// slackMessage is the payload format for Slack incoming webhooks.
type slackMessage struct {
	Text string `json:"text"`
}

// WebhookSender delivers alert notifications to HTTP webhook endpoints.
type WebhookSender struct {
	client *http.Client
	logger *slog.Logger
}

// NewWebhookSender creates a WebhookSender with a 10-second timeout.
func NewWebhookSender(logger *slog.Logger) *WebhookSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookSender{
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// Send delivers a webhook payload to all supplied URLs.
// It retries failed requests up to 3 times with exponential backoff (1s, 2s, 4s).
func (w *WebhookSender) Send(ctx context.Context, urls []string, status AlertStatus, alerts []Alert) {
	if len(urls) == 0 || len(alerts) == 0 {
		return
	}

	payload := WebhookPayload{
		Version: "1",
		Status:  string(status),
		Alerts:  alerts,
	}

	for _, url := range urls {
		w.sendOne(ctx, url, payload)
	}
}

func (w *WebhookSender) sendOne(ctx context.Context, url string, payload WebhookPayload) {
	var body []byte
	var err error

	if isSlackURL(url) {
		body, err = buildSlackBody(payload)
	} else {
		body, err = json.Marshal(payload)
	}
	if err != nil {
		w.logger.Error("failed to marshal webhook payload", "url", url, "error", err)
		return
	}

	backoff := time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				w.logger.Warn("webhook send cancelled", "url", url, "error", ctx.Err())
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if reqErr != nil {
			w.logger.Error("failed to create webhook request", "url", url, "error", reqErr)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := w.client.Do(req)
		if doErr != nil {
			w.logger.Warn("webhook request failed", "url", url, "attempt", attempt+1, "error", doErr)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			w.logger.Debug("webhook delivered", "url", url, "status", resp.StatusCode)
			return
		}

		w.logger.Warn("webhook returned non-2xx", "url", url, "attempt", attempt+1, "status", resp.StatusCode)
	}

	w.logger.Error("webhook delivery exhausted retries", "url", url)
}

func isSlackURL(url string) bool {
	return strings.Contains(url, "hooks.slack.com")
}

func buildSlackBody(payload WebhookPayload) ([]byte, error) {
	var sb strings.Builder
	icon := ":rotating_light:"
	if payload.Status == string(AlertResolved) {
		icon = ":white_check_mark:"
	}

	sb.WriteString(fmt.Sprintf("%s *Alert %s*\n", icon, payload.Status))

	for _, a := range payload.Alerts {
		sb.WriteString(fmt.Sprintf("• *%s* [%s] value=%.4g\n", a.RuleName, a.Severity, a.Value))
		if msg, ok := a.Annotations["summary"]; ok {
			sb.WriteString(fmt.Sprintf("  %s\n", msg))
		}
	}

	msg := slackMessage{Text: sb.String()}
	return json.Marshal(msg)
}
