package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

// CPClient communicates with the control plane's HTTP API.
type CPClient struct {
	baseURL string
	token   string
	http    *http.Client
	logger  *slog.Logger
}

// CPClientTLS configures TLS for the worker-to-CP connection.
type CPClientTLS struct {
	CACertPath string // Path to CA certificate for server verification.
	Insecure   bool   // Skip TLS verification (not recommended).
}

// NewCPClient creates a new control plane client with optional TLS.
func NewCPClient(baseURL, token string, tlsOpts *CPClientTLS, logger *slog.Logger) *CPClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if tlsOpts != nil {
		tlsCfg := &tls.Config{}
		if tlsOpts.Insecure {
			tlsCfg.InsecureSkipVerify = true
		} else if tlsOpts.CACertPath != "" {
			caCert, err := os.ReadFile(tlsOpts.CACertPath)
			if err == nil {
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(caCert)
				tlsCfg.RootCAs = pool
			}
		}
		transport.TLSClientConfig = tlsCfg
	}

	return &CPClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second, Transport: transport},
		logger:  logger,
	}
}

func (c *CPClient) post(ctx context.Context, path string, body any, result any) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}

	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s.
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		var bodyReader io.Reader
		if data != nil {
			bodyReader = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bodyReader)
		if err != nil {
			return err
		}
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err // Context cancelled, don't retry.
			}
			c.logger.Warn("CP request failed, retrying", "path", path, "attempt", attempt+1, "error", err)
			continue
		}
		defer resp.Body.Close()

		// 5xx errors are retryable; 4xx are not.
		if resp.StatusCode >= 500 {
			var errResp map[string]string
			json.NewDecoder(resp.Body).Decode(&errResp)
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, errResp["error"])
			c.logger.Warn("CP server error, retrying", "path", path, "status", resp.StatusCode, "attempt", attempt+1)
			continue
		}
		if resp.StatusCode >= 400 {
			var errResp map[string]string
			json.NewDecoder(resp.Body).Decode(&errResp)
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errResp["error"])
		}
		if result != nil {
			return json.NewDecoder(resp.Body).Decode(result)
		}
		return nil
	}
	return lastErr
}

// Register registers this worker with the control plane.
func (c *CPClient) Register(ctx context.Context, hostname, provider string, capacity loka.ResourceCapacity, labels map[string]string) (string, error) {
	req := map[string]any{
		"hostname": hostname,
		"provider": provider,
		"capacity": capacity,
		"labels":   labels,
	}
	var resp struct {
		WorkerID string `json:"worker_id"`
	}
	// For MVP, we'll use the internal registration endpoint.
	// In production, this is a gRPC call.
	err := c.post(ctx, "/api/internal/workers/register", req, &resp)
	return resp.WorkerID, err
}

// SendHeartbeat sends a heartbeat to the control plane.
// Returns "unknown_worker" status if the CP doesn't recognize this worker.
func (c *CPClient) SendHeartbeat(ctx context.Context, hb loka.Heartbeat) (string, error) {
	var resp struct {
		Status string `json:"status"`
	}
	err := c.post(ctx, "/api/internal/workers/heartbeat", map[string]any{
		"worker_id":     hb.WorkerID,
		"status":        string(hb.Status),
		"session_count": hb.SessionCount,
		"session_ids":   hb.SessionIDs,
		"usage":         hb.Usage,
	}, &resp)
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}

// ReportExecComplete reports execution results to the control plane.
func (c *CPClient) ReportExecComplete(ctx context.Context, sessionID, execID string, status loka.ExecStatus, results []loka.CommandResult, errMsg string) error {
	return c.post(ctx, "/api/internal/exec/complete", map[string]any{
		"session_id": sessionID,
		"exec_id":    execID,
		"status":     string(status),
		"results":    results,
		"error":      errMsg,
	}, nil)
}

// ReportSessionStatus reports session status to the control plane.
func (c *CPClient) ReportSessionStatus(ctx context.Context, sessionID string, status loka.SessionStatus) error {
	return c.post(ctx, "/api/internal/sessions/status", map[string]any{
		"session_id": sessionID,
		"status":     string(status),
	}, nil)
}
