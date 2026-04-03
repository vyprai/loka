package volrepl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vyprai/loka/internal/worker/volsync"
)

// Client talks to a peer worker's replication HTTP endpoint.
type Client struct {
	baseURL    string // e.g., "http://10.0.1.5:8081"
	httpClient *http.Client
}

// NewClient creates a replication client for a peer worker.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FetchManifest gets the current manifest from the peer.
func (c *Client) FetchManifest(ctx context.Context, volName string) (*volsync.Manifest, error) {
	u := fmt.Sprintf("%s/volrepl/%s/manifest", c.baseURL, url.PathEscape(volName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: status %d", resp.StatusCode)
	}
	var m volsync.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// DownloadFile downloads a file from the peer to a local path.
func (c *Client) DownloadFile(ctx context.Context, volName, relPath, localPath string) error {
	u := fmt.Sprintf("%s/volrepl/%s/file?path=%s", c.baseURL,
		url.PathEscape(volName), url.QueryEscape(relPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download file: status %d", resp.StatusCode)
	}

	os.MkdirAll(filepath.Dir(localPath), 0o755)
	tmpPath := localPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, localPath)
}

// PeerSyncTarget implements volsync.SyncTarget for pushing data to a peer worker.
type PeerSyncTarget struct {
	client *Client
}

// NewPeerSyncTarget creates a SyncTarget that pushes to a peer worker.
func NewPeerSyncTarget(baseURL string) *PeerSyncTarget {
	return &PeerSyncTarget{client: NewClient(baseURL)}
}

func (p *PeerSyncTarget) UploadFile(ctx context.Context, volName, relPath string, r io.Reader, size int64) error {
	u := fmt.Sprintf("%s/volrepl/%s/file?path=%s", p.client.baseURL,
		url.PathEscape(volName), url.QueryEscape(relPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := p.client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("push file: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("push file: status %d", resp.StatusCode)
	}
	return nil
}

func (p *PeerSyncTarget) DeleteFile(ctx context.Context, volName, relPath string) error {
	u := fmt.Sprintf("%s/volrepl/%s/file?path=%s", p.client.baseURL,
		url.PathEscape(volName), url.QueryEscape(relPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	resp.Body.Close()
	return nil
}

func (p *PeerSyncTarget) SaveManifest(ctx context.Context, volName string, manifest *volsync.Manifest) error {
	// Manifest is rebuilt from local files on the replica side — no need to push.
	return nil
}

func (p *PeerSyncTarget) FetchManifest(ctx context.Context, volName string) (*volsync.Manifest, error) {
	return p.client.FetchManifest(ctx, volName)
}

func (p *PeerSyncTarget) DownloadFile(ctx context.Context, volName, relPath, localPath string) error {
	return p.client.DownloadFile(ctx, volName, relPath, localPath)
}
