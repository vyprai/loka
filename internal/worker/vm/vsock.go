package vm

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
)

// VsockClient communicates with the supervisor inside a VM via JSON-RPC.
// Uses lokavm's DialVsock function for direct vsock connections.
// The supervisor listens on vsock port 52 inside the guest.
type VsockClient struct {
	socketPath string                              // Legacy UDS path (unused with lokavm).
	dialFn     func(port uint32) (net.Conn, error) // Vsock dialer from VM.DialVsock.
	timeout    time.Duration

	// Connection pool: reuse a persistent connection for lower latency.
	mu       sync.Mutex
	conn     net.Conn
	connGood bool
}

// NewVsockClientFromDialer creates a client using a direct vsock dial function.
func NewVsockClientFromDialer(dialFn func(port uint32) (net.Conn, error)) *VsockClient {
	return &VsockClient{
		dialFn:  dialFn,
		timeout: 30 * time.Second,
	}
}

// SocketPath returns the underlying vsock UDS path (empty for lokavm dialer).
func (c *VsockClient) SocketPath() string {
	return c.socketPath
}

// Close closes the pooled vsock connection.
func (c *VsockClient) Close() {
	c.closeConn()
}

// ── RPC Messages ────────────────────────────────────────

// RPCRequest is sent from the worker (host) to the supervisor (guest).
type RPCRequest struct {
	Method string          `json:"method"`
	ID     string          `json:"id"`
	Params json.RawMessage `json:"params"`
}

// RPCResponse is sent from the supervisor (guest) to the worker (host).
type RPCResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is an error in an RPC response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── High-Level Methods ──────────────────────────────────

// ExecRequest is the params for "exec" method.
type ExecRequest struct {
	Commands []loka.Command `json:"commands"`
	Parallel bool           `json:"parallel"`
	ExecID   string         `json:"exec_id"`
}

// ExecResponse is the result of an "exec" method.
type ExecResponse struct {
	Status  string               `json:"status"` // "success", "failed", "pending_approval"
	Results []loka.CommandResult `json:"results"`
	Error   string               `json:"error,omitempty"`
}

// Execute sends commands to the supervisor for execution.
func (c *VsockClient) Execute(req ExecRequest) (*ExecResponse, error) {
	params, _ := json.Marshal(req)
	resp, err := c.call("exec", params)
	if err != nil {
		return nil, err
	}
	var result ExecResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal exec response: %w", err)
	}
	return &result, nil
}

// SetMode changes the execution mode inside the VM.
func (c *VsockClient) SetMode(mode loka.ExecMode) error {
	params, _ := json.Marshal(map[string]string{"mode": string(mode)})
	_, err := c.call("set_mode", params)
	return err
}

// SetPolicy updates the exec policy inside the VM.
func (c *VsockClient) SetPolicy(policy loka.ExecPolicy) error {
	params, _ := json.Marshal(policy)
	_, err := c.call("set_policy", params)
	return err
}

// ApproveCommand approves a suspended command at the gate.
func (c *VsockClient) ApproveCommand(cmdID string, addToWhitelist bool) error {
	params, _ := json.Marshal(map[string]any{
		"command_id":       cmdID,
		"add_to_whitelist": addToWhitelist,
	})
	_, err := c.call("approve", params)
	return err
}

// DenyCommand denies a suspended command at the gate.
func (c *VsockClient) DenyCommand(cmdID, reason string) error {
	params, _ := json.Marshal(map[string]string{
		"command_id": cmdID,
		"reason":     reason,
	})
	_, err := c.call("deny", params)
	return err
}

// ListPending returns commands waiting for approval.
func (c *VsockClient) ListPending() ([]PendingInfo, error) {
	resp, err := c.call("list_pending", nil)
	if err != nil {
		return nil, err
	}
	var result []PendingInfo
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal list_pending response: %w", err)
	}
	return result, nil
}

// PendingInfo describes a command waiting for approval.
type PendingInfo struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Reason  string `json:"reason"`
}

// Ping checks if the supervisor is alive.
func (c *VsockClient) Ping() error {
	_, err := c.call("ping", nil)
	return err
}

// ── Service Methods ─────────────────────────────────────

// ServiceStatusResult describes the current state of the service.
type ServiceStatusResult struct {
	Running       bool    `json:"running"`
	PID           int     `json:"pid"`
	ExitCode      int     `json:"exit_code"`
	Restarts      int     `json:"restarts"`
	UptimeSeconds float64 `json:"uptime_seconds"`
	StartedAt     string  `json:"started_at"`
}

// ServiceLogsResult contains the last N lines of service stdout/stderr.
type ServiceLogsResult struct {
	Stdout []string `json:"stdout"`
	Stderr []string `json:"stderr"`
}

// ServiceStart starts a long-running service process inside the VM.
// Only one service per VM is supported. Returns the process PID.
func (c *VsockClient) ServiceStart(command string, args []string, env map[string]string, workdir, restartPolicy string) (int, error) {
	params, _ := json.Marshal(map[string]any{
		"command":        command,
		"args":           args,
		"env":            env,
		"workdir":        workdir,
		"restart_policy": restartPolicy,
	})
	resp, err := c.call("service_start", params)
	if err != nil {
		return 0, err
	}
	var result struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return 0, fmt.Errorf("unmarshal service_start response: %w", err)
	}
	return result.PID, nil
}

// ServiceStop stops the running service process.
func (c *VsockClient) ServiceStop(signal string, timeout int) error {
	params, _ := json.Marshal(map[string]any{
		"signal":  signal,
		"timeout": timeout,
	})
	_, err := c.call("service_stop", params)
	return err
}

// ServiceStatus returns the current state of the service process.
func (c *VsockClient) ServiceStatus() (*ServiceStatusResult, error) {
	resp, err := c.call("service_status", nil)
	if err != nil {
		return nil, err
	}
	var result ServiceStatusResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal service_status response: %w", err)
	}
	return &result, nil
}

// ServiceLogs returns the last N lines of service stdout/stderr.
func (c *VsockClient) ServiceLogs(lines int) (*ServiceLogsResult, error) {
	params, _ := json.Marshal(map[string]int{"lines": lines})
	resp, err := c.call("service_logs", params)
	if err != nil {
		return nil, err
	}
	var result ServiceLogsResult
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal service_logs response: %w", err)
	}
	return &result, nil
}

// MountVolumeRequest is the params for "mount_volume" method.
type MountVolumeRequest struct {
	Path     string `json:"path"`                // Mount path inside VM.
	Mode     string `json:"mode"`                // "virtiofs", "fuse", or "block".
	ReadOnly bool   `json:"readonly"`            // Whether the mount is read-only.
	Tag      string `json:"tag,omitempty"`        // Virtiofs mount tag (for virtiofs mode).
	Bucket   string `json:"bucket,omitempty"`     // Object store bucket (for fuse mode).
	Prefix   string `json:"prefix,omitempty"`     // Object key prefix (for fuse mode).
}

// MountVolume sends a mount_volume RPC to the supervisor inside the VM.
// For FUSE mode, the supervisor mounts a FUSE filesystem backed by vsock RPCs to the host.
// For block mode, the supervisor mounts the pre-attached /dev/vdX drive.
func (c *VsockClient) MountVolume(req MountVolumeRequest) error {
	params, _ := json.Marshal(req)
	_, err := c.call("mount_volume", params)
	return err
}

// WriteHostsEntries sends a write_hosts RPC to inject /etc/hosts entries inside the VM.
// Used for inter-component service discovery within multi-component services.
func (c *VsockClient) WriteHostsEntries(entries map[string]string) error {
	params, _ := json.Marshal(map[string]any{"entries": entries})
	_, err := c.call("write_hosts", params)
	return err
}

// FsStatRequest is the params for "fs_stat" host-side RPC.
type FsStatRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// FsStatResult is the result of an "fs_stat" RPC.
type FsStatResult struct {
	Exists bool  `json:"exists"`
	Size   int64 `json:"size"`
	IsDir  bool  `json:"is_dir"`
}

// FsReadRequest is the params for "fs_read" host-side RPC.
type FsReadRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

// FsReadResult is the result of an "fs_read" RPC.
type FsReadResult struct {
	Data string `json:"data"` // Base64-encoded file content.
	Size int64  `json:"size"` // Total file size.
}

// FsWriteRequest is the params for "fs_write" host-side RPC.
type FsWriteRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Data   string `json:"data"` // Base64-encoded file content.
}

// FsListRequest is the params for "fs_list" host-side RPC.
type FsListRequest struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

// FsListEntry describes a single file/directory in a listing.
type FsListEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// FsDeleteRequest is the params for "fs_delete" host-side RPC.
type FsDeleteRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// HealthCheck checks if a port is listening inside the VM, optionally doing an
// HTTP GET if path is non-empty. Uses the supervisor's health_check RPC.
func (c *VsockClient) HealthCheck(port int, path string) error {
	params, _ := json.Marshal(map[string]any{
		"port": port,
		"path": path,
	})
	_, err := c.call("health_check", params)
	return err
}

// ── Low-Level Transport ─────────────────────────────────

// TODO: Each RPC call creates a new vsock connection, which adds latency.
// A future improvement would be connection pooling — maintain a pool of
// pre-established connections and reuse them across calls. For now, each
// connection is properly closed via defer conn.Close() to prevent leaks.

func (c *VsockClient) call(method string, params json.RawMessage) (json.RawMessage, error) {
	// Try pooled connection first, fall back to fresh dial.
	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(c.timeout))

	req := RPCRequest{
		Method: method,
		ID:     uuid.New().String(),
		Params: params,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		// Connection broken — close and retry with fresh one.
		c.closeConn()
		conn, err = c.getConn()
		if err != nil {
			return nil, err
		}
		conn.SetDeadline(time.Now().Add(c.timeout))
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			c.closeConn()
			return nil, fmt.Errorf("vsock write: %w", err)
		}
	}

	var rpcResp RPCResponse
	if err := json.NewDecoder(conn).Decode(&rpcResp); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("vsock read: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("supervisor error: %s", rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

// getConn returns a pooled connection or dials a new one.
func (c *VsockClient) getConn() (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil && c.connGood {
		return c.conn, nil
	}

	conn, err := c.dialNew()
	if err != nil {
		return nil, err
	}
	c.conn = conn
	c.connGood = true
	return conn, nil
}

// closeConn marks the pooled connection as broken.
func (c *VsockClient) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.connGood = false
	}
}

// dialNew establishes a fresh connection to the supervisor.
func (c *VsockClient) dialNew() (net.Conn, error) {
	// lokavm path: direct vsock dial.
	if c.dialFn != nil {
		conn, err := c.dialFn(52) // Supervisor listens on vsock port 52.
		if err != nil {
			return nil, fmt.Errorf("vsock dial: %w", err)
		}
		return conn, nil
	}

	// Firecracker path: UDS + CONNECT handshake.
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("vsock connect: %w", err)
	}
	conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := fmt.Fprintf(conn, "CONNECT 52\n"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake read: %w", err)
	}
	resp := string(buf[:n])
	if len(resp) < 2 || resp[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake failed: %s", resp)
	}

	return conn, nil
}
