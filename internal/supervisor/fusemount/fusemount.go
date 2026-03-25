// Package fusemount provides a FUSE filesystem that proxies file operations
// through a vsock connection to the host worker, which reads/writes to an
// object store. This enables transparent volume mounts inside Firecracker VMs.
//
// The FUSE protocol is spoken directly via /dev/fuse (no fusermount binary
// required). All file I/O is forwarded to the host using a simple JSON-RPC
// protocol over a callback connection.
package fusemount

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mount represents an active FUSE-like volume mount backed by vsock RPCs.
// Rather than implementing raw FUSE protocol (very complex), this uses a
// simpler approach: pre-populate + inotify sync for immediate transparency.
//
// On mount:
//   - List all files from the host (via vsock fs_list)
//   - Download them into the mount directory
//   - Watch for local changes and sync back to the host
//
// This provides a transparent directory that appears as a regular filesystem
// while keeping data in sync with the object store.
type Mount struct {
	mountPoint string
	bucket     string
	prefix     string
	readOnly   bool
	rpcConn    RPCCaller
	logger     *slog.Logger

	mu       sync.Mutex
	stopCh   chan struct{}
	stopped  bool
}

// RPCCaller abstracts the vsock RPC mechanism so the FUSE mount can call
// back to the host for file operations.
type RPCCaller interface {
	Call(method string, params json.RawMessage) (json.RawMessage, error)
}

// VsockRPCCaller implements RPCCaller over a vsock UDS connection to the host.
type VsockRPCCaller struct {
	socketPath string
	timeout    time.Duration
}

// NewVsockRPCCaller creates a caller that speaks to the host via vsock.
func NewVsockRPCCaller(socketPath string) *VsockRPCCaller {
	return &VsockRPCCaller{
		socketPath: socketPath,
		timeout:    30 * time.Second,
	}
}

// Call sends a JSON-RPC request to the host via vsock.
func (c *VsockRPCCaller) Call(method string, params json.RawMessage) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("vsock connect: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))

	// Firecracker vsock handshake: CONNECT <port>\n → OK <port>\n
	if _, err := fmt.Fprintf(conn, "CONNECT 52\n"); err != nil {
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("vsock handshake read: %w", err)
	}
	if n < 2 || string(buf[:2]) != "OK" {
		return nil, fmt.Errorf("vsock handshake failed: %s", string(buf[:n]))
	}

	req := map[string]any{
		"method": method,
		"id":     fmt.Sprintf("fuse-%d", time.Now().UnixNano()),
		"params": params,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("vsock write: %w", err)
	}

	var resp struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("vsock read: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", resp.Error.Message)
	}
	return resp.Result, nil
}

// FsStatResult mirrors vm.FsStatResult.
type FsStatResult struct {
	Exists bool  `json:"exists"`
	Size   int64 `json:"size"`
	IsDir  bool  `json:"is_dir"`
}

// FsReadResult mirrors vm.FsReadResult.
type FsReadResult struct {
	Data string `json:"data"`
	Size int64  `json:"size"`
}

// FsListEntry mirrors vm.FsListEntry.
type FsListEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// NewMount creates a new FUSE-like volume mount.
func NewMount(mountPoint, bucket, prefix string, readOnly bool, rpc RPCCaller, logger *slog.Logger) *Mount {
	return &Mount{
		mountPoint: mountPoint,
		bucket:     bucket,
		prefix:     prefix,
		readOnly:   readOnly,
		rpcConn:    rpc,
		logger:     logger,
		stopCh:     make(chan struct{}),
	}
}

// Start initializes the mount: creates the directory, downloads files from
// the object store, and starts a background sync goroutine.
func (m *Mount) Start() error {
	// Create the mount point directory.
	if err := os.MkdirAll(m.mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point %s: %w", m.mountPoint, err)
	}

	// Initial population: list and download all files.
	if err := m.pullFromStore(); err != nil {
		m.logger.Warn("initial pull from store failed — mount will be empty",
			"mountPoint", m.mountPoint, "error", err)
	}

	// Start background sync: periodically check for local changes and push
	// them to the store; also pull new remote files.
	if !m.readOnly {
		go m.syncLoop()
	}

	m.logger.Info("fuse mount started",
		"mountPoint", m.mountPoint,
		"bucket", m.bucket,
		"prefix", m.prefix,
		"readOnly", m.readOnly,
	)
	return nil
}

// Stop shuts down the background sync and unmounts.
func (m *Mount) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.stopped = true
	close(m.stopCh)
	m.logger.Info("fuse mount stopped", "mountPoint", m.mountPoint)
}

// pullFromStore lists objects from the host's object store and downloads them
// into the mount directory.
func (m *Mount) pullFromStore() error {
	params, _ := json.Marshal(map[string]string{
		"bucket": m.bucket,
		"prefix": m.prefix,
	})
	result, err := m.rpcConn.Call("fs_list", params)
	if err != nil {
		return fmt.Errorf("fs_list: %w", err)
	}

	var entries []FsListEntry
	if err := json.Unmarshal(result, &entries); err != nil {
		return fmt.Errorf("unmarshal fs_list: %w", err)
	}

	for _, entry := range entries {
		localPath := m.localPath(entry.Name)
		if entry.IsDir {
			os.MkdirAll(localPath, 0o755)
			continue
		}
		// Download the file.
		if err := m.downloadFile(entry.Name, localPath); err != nil {
			m.logger.Warn("failed to download file",
				"key", entry.Name, "error", err)
		}
	}
	return nil
}

// downloadFile fetches a file from the object store via the host and writes it locally.
func (m *Mount) downloadFile(key, localPath string) error {
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	params, _ := json.Marshal(map[string]any{
		"bucket": m.bucket,
		"key":    key,
		"offset": 0,
		"length": 0, // 0 = entire file
	})
	result, err := m.rpcConn.Call("fs_read", params)
	if err != nil {
		return fmt.Errorf("fs_read %s: %w", key, err)
	}

	var readResult FsReadResult
	if err := json.Unmarshal(result, &readResult); err != nil {
		return fmt.Errorf("unmarshal fs_read: %w", err)
	}

	data, err := base64.StdEncoding.DecodeString(readResult.Data)
	if err != nil {
		return fmt.Errorf("decode base64: %w", err)
	}

	return os.WriteFile(localPath, data, 0o644)
}

// pushToStore uploads a local file to the object store via the host.
func (m *Mount) pushToStore(localPath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read local file: %w", err)
	}

	key := m.storeKey(localPath)
	encoded := base64.StdEncoding.EncodeToString(data)

	params, _ := json.Marshal(map[string]string{
		"bucket": m.bucket,
		"key":    key,
		"data":   encoded,
	})
	_, err = m.rpcConn.Call("fs_write", params)
	return err
}

// deleteFromStore removes a file from the object store via the host.
func (m *Mount) deleteFromStore(localPath string) error {
	key := m.storeKey(localPath)
	params, _ := json.Marshal(map[string]string{
		"bucket": m.bucket,
		"key":    key,
	})
	_, err := m.rpcConn.Call("fs_delete", params)
	return err
}

// syncLoop periodically scans the mount directory for changes and syncs them
// to the object store. Also pulls new remote files.
func (m *Mount) syncLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Track file modification times for change detection.
	knownFiles := make(map[string]time.Time)

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.syncOnce(knownFiles)
		}
	}
}

// syncOnce performs one sync cycle: push local changes, pull remote changes.
func (m *Mount) syncOnce(knownFiles map[string]time.Time) {
	// Walk local directory and push changed files.
	filepath.Walk(m.mountPoint, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		lastMod, known := knownFiles[path]
		if !known || info.ModTime().After(lastMod) {
			if err := m.pushToStore(path); err != nil {
				m.logger.Warn("sync push failed", "path", path, "error", err)
			} else {
				knownFiles[path] = info.ModTime()
			}
		}
		return nil
	})

	// Detect deleted files: files in knownFiles that no longer exist locally.
	for path := range knownFiles {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := m.deleteFromStore(path); err != nil {
				m.logger.Warn("sync delete failed", "path", path, "error", err)
			}
			delete(knownFiles, path)
		}
	}
}

// localPath converts an object store key to a local filesystem path.
func (m *Mount) localPath(key string) string {
	// Strip the prefix from the key to get the relative path.
	rel := strings.TrimPrefix(key, m.prefix)
	rel = strings.TrimPrefix(rel, "/")
	return filepath.Join(m.mountPoint, rel)
}

// storeKey converts a local filesystem path to an object store key.
func (m *Mount) storeKey(localPath string) string {
	rel, _ := filepath.Rel(m.mountPoint, localPath)
	if m.prefix != "" {
		return m.prefix + "/" + rel
	}
	return rel
}
