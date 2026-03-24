package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/objstore"
)

// Store implements objstore.ObjectStore by proxying all operations to a remote
// control plane via HTTP. Used by:
//   - Workers writing data through the leader CP
//   - HA non-leader CPs forwarding writes to the leader
type Store struct {
	mu         sync.RWMutex
	baseURL    string       // e.g. "https://leader:6840"
	httpClient *http.Client
	token      string // Worker token or internal auth token.
}

// Config configures the proxy object store.
type Config struct {
	BaseURL  string // Control plane base URL.
	Token    string // Authentication token.
	Insecure bool   // Skip TLS verification.
}

// New creates a new proxy object store.
func New(cfg Config) *Store {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	return &Store{
		baseURL: cfg.BaseURL,
		token:   cfg.Token,
		httpClient: &http.Client{
			Timeout:   5 * time.Minute, // Large objects can take time.
			Transport: transport,
		},
	}
}

func (s *Store) url(path string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseURL + "/api/internal/objstore" + path
}

func (s *Store) do(req *http.Request) (*http.Response, error) {
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	return s.httpClient.Do(req)
}

func (s *Store) Put(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	u := fmt.Sprintf("%s/%s/%s", s.url("/objects"), url.PathEscape(bucket), url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := s.do(req)
	if err != nil {
		return fmt.Errorf("proxy put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("proxy put: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/%s/%s", s.url("/objects"), url.PathEscape(bucket), url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := s.do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy get: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("not found: %s/%s", bucket, key)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("proxy get: HTTP %d: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	u := fmt.Sprintf("%s/%s/%s", s.url("/objects"), url.PathEscape(bucket), url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := s.do(req)
	if err != nil {
		return fmt.Errorf("proxy delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("proxy delete: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

func (s *Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	u := fmt.Sprintf("%s/%s/%s", s.url("/objects"), url.PathEscape(bucket), url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}

	resp, err := s.do(req)
	if err != nil {
		return false, fmt.Errorf("proxy exists: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func (s *Store) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	u := fmt.Sprintf("%s/%s/%s?presign=true&expiry=%s", s.url("/objects"), url.PathEscape(bucket), url.PathEscape(key), expiry)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.do(req)
	if err != nil {
		return "", fmt.Errorf("proxy presign: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("proxy presign: HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode presign response: %w", err)
	}
	return result.URL, nil
}

func (s *Store) List(ctx context.Context, bucket, prefix string) ([]objstore.ObjectInfo, error) {
	u := fmt.Sprintf("%s/%s?prefix=%s", s.url("/list"), url.PathEscape(bucket), url.QueryEscape(prefix))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := s.do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("proxy list: HTTP %d: %s", resp.StatusCode, body)
	}

	var objects []objstore.ObjectInfo
	if err := json.NewDecoder(resp.Body).Decode(&objects); err != nil {
		// Empty response is fine.
		if err == io.EOF {
			return nil, nil
		}
		return nil, fmt.Errorf("decode list response: %w", err)
	}
	return objects, nil
}

// SetBaseURL updates the base URL (used when leader changes in HA).
// Thread-safe: protected by mutex since this may be called during request handling.
func (s *Store) SetBaseURL(baseURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseURL = baseURL
}

// readBody reads the full response body as bytes, for use in proxy writes where
// we need to buffer to know size. Unused for now but available.
func readBody(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	return buf.Bytes(), err
}

var _ objstore.ObjectStore = (*Store)(nil)
