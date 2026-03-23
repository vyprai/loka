package lokaapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client is an HTTP client for the LOKA control plane API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

// NewClient creates a new API client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		token: token,
	}
}

// TLSOptions configures TLS for the client.
type TLSOptions struct {
	CACertPath string // Path to CA certificate file.
	Insecure   bool   // Skip TLS verification.
}

// NewClientWithTLS creates a client with custom TLS settings.
func NewClientWithTLS(baseURL, token string, opts TLSOptions) (*Client, error) {
	tlsCfg := &tls.Config{}

	if opts.Insecure {
		tlsCfg.InsecureSkipVerify = true
	} else if opts.CACertPath != "" {
		caCert, err := os.ReadFile(opts.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("invalid CA certificate")
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
			},
		},
		token: token,
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp map[string]string
		json.NewDecoder(resp.Body).Decode(&errResp)
		msg := errResp["error"]
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", msg)
	}

	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ─── Sessions ───────────────────────────────────────────

type Session struct {
	ID        string            `json:"ID"`
	Name      string            `json:"Name"`
	Status    string            `json:"Status"`
	Mode      string            `json:"Mode"`
	WorkerID  string            `json:"WorkerID"`
	ImageRef  string            `json:"ImageRef"`
	VCPUs     int               `json:"VCPUs"`
	MemoryMB  int               `json:"MemoryMB"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt time.Time         `json:"CreatedAt"`
	UpdatedAt time.Time         `json:"UpdatedAt"`
}

type CreateSessionReq struct {
	Name            string            `json:"name"`
	Image           string            `json:"image,omitempty"`       // Docker image: "ubuntu:22.04"
	SnapshotID      string            `json:"snapshot_id,omitempty"` // Restore from snapshot.
	Mode            string            `json:"mode,omitempty"`
	VCPUs           int               `json:"vcpus,omitempty"`
	MemoryMB        int               `json:"memory_mb,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	AllowedCommands []string          `json:"allowed_commands,omitempty"`
	BlockedCommands []string          `json:"blocked_commands,omitempty"`
}

func (c *Client) CreateSession(ctx context.Context, req CreateSessionReq) (*Session, error) {
	var s Session
	err := c.do(ctx, "POST", "/api/v1/sessions", req, &s)
	return &s, err
}

func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := c.do(ctx, "GET", "/api/v1/sessions/"+id, nil, &s)
	return &s, err
}

type ListSessionsResp struct {
	Sessions []Session `json:"sessions"`
	Total    int       `json:"total"`
}

func (c *Client) ListSessions(ctx context.Context) (*ListSessionsResp, error) {
	var resp ListSessionsResp
	err := c.do(ctx, "GET", "/api/v1/sessions", nil, &resp)
	return &resp, err
}

func (c *Client) DestroySession(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/v1/sessions/"+id, nil, nil)
}

func (c *Client) PauseSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := c.do(ctx, "POST", "/api/v1/sessions/"+id+"/pause", nil, &s)
	return &s, err
}

func (c *Client) ResumeSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := c.do(ctx, "POST", "/api/v1/sessions/"+id+"/resume", nil, &s)
	return &s, err
}

func (c *Client) SetSessionMode(ctx context.Context, id, mode string) (*Session, error) {
	var s Session
	err := c.do(ctx, "POST", "/api/v1/sessions/"+id+"/mode", map[string]string{"mode": mode}, &s)
	return &s, err
}

// ─── Execution ──────────────────────────────────────────

type Execution struct {
	ID        string          `json:"ID"`
	SessionID string          `json:"SessionID"`
	Status    string          `json:"Status"`
	Parallel  bool            `json:"Parallel"`
	Commands  json.RawMessage `json:"Commands"`
	Results   json.RawMessage `json:"Results"`
	CreatedAt time.Time       `json:"CreatedAt"`
	UpdatedAt time.Time       `json:"UpdatedAt"`
}

type ExecReq struct {
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Commands []ExecCommand     `json:"commands,omitempty"`
	Parallel bool              `json:"parallel,omitempty"`
}

type ExecCommand struct {
	ID      string            `json:"id,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func (c *Client) Exec(ctx context.Context, sessionID string, req ExecReq) (*Execution, error) {
	var e Execution
	err := c.do(ctx, "POST", "/api/v1/sessions/"+sessionID+"/exec", req, &e)
	return &e, err
}

func (c *Client) GetExecution(ctx context.Context, sessionID, execID string) (*Execution, error) {
	var e Execution
	err := c.do(ctx, "GET", "/api/v1/sessions/"+sessionID+"/exec/"+execID, nil, &e)
	return &e, err
}

// ─── Checkpoints ────────────────────────────────────────

type Checkpoint struct {
	ID        string    `json:"ID"`
	SessionID string    `json:"SessionID"`
	ParentID  string    `json:"ParentID"`
	Type      string    `json:"Type"`
	Status    string    `json:"Status"`
	Label     string    `json:"Label"`
	CreatedAt time.Time `json:"CreatedAt"`
}

type CreateCheckpointReq struct {
	Type  string `json:"type,omitempty"`
	Label string `json:"label,omitempty"`
}

func (c *Client) CreateCheckpoint(ctx context.Context, sessionID string, req CreateCheckpointReq) (*Checkpoint, error) {
	var cp Checkpoint
	err := c.do(ctx, "POST", "/api/v1/sessions/"+sessionID+"/checkpoints", req, &cp)
	return &cp, err
}

type ListCheckpointsResp struct {
	SessionID   string       `json:"session_id"`
	Root        string       `json:"root"`
	Current     string       `json:"current"`
	Checkpoints []Checkpoint `json:"checkpoints"`
}

func (c *Client) ListCheckpoints(ctx context.Context, sessionID string) (*ListCheckpointsResp, error) {
	var resp ListCheckpointsResp
	err := c.do(ctx, "GET", "/api/v1/sessions/"+sessionID+"/checkpoints", nil, &resp)
	return &resp, err
}

func (c *Client) RestoreCheckpoint(ctx context.Context, sessionID, cpID string) (*Session, error) {
	var s Session
	err := c.do(ctx, "POST", "/api/v1/sessions/"+sessionID+"/checkpoints/"+cpID+"/restore", nil, &s)
	return &s, err
}

// ─── Raw ────────────────────────────────────────────────

// Raw performs a raw API call for endpoints not covered by typed methods.
func (c *Client) Raw(ctx context.Context, method, path string, body any, result any) error {
	return c.do(ctx, method, path, body, result)
}

// ─── Health ─────────────────────────────────────────────

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, "GET", "/api/v1/health", nil, nil)
}
