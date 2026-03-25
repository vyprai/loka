package vm

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
)

// VsockClient communicates with the supervisor inside a Firecracker VM
// over the vsock unix domain socket exposed by Firecracker on the host.
//
// Protocol: JSON-RPC over a single UDS connection.
// The supervisor listens on vsock port 52 inside the guest.
// Firecracker exposes it as a UDS file on the host.
type VsockClient struct {
	socketPath string
	timeout    time.Duration
}

// NewVsockClient creates a client for a VM's vsock socket.
func NewVsockClient(vsockPath string) *VsockClient {
	return &VsockClient{
		socketPath: vsockPath,
		timeout:    30 * time.Second,
	}
}

// SocketPath returns the underlying vsock UDS path, allowing callers to
// establish raw connections for operations like TCP port forwarding.
func (c *VsockClient) SocketPath() string {
	return c.socketPath
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
	Path     string `json:"path"`      // Mount path inside VM.
	Mode     string `json:"mode"`      // "fuse" or "block".
	ReadOnly bool   `json:"readonly"`  // Whether the mount is read-only.
	Bucket   string `json:"bucket"`    // Object store bucket (for fuse mode).
	Prefix   string `json:"prefix"`    // Object key prefix (for fuse mode).
}

// MountVolume sends a mount_volume RPC to the supervisor inside the VM.
// For FUSE mode, the supervisor mounts a FUSE filesystem backed by vsock RPCs to the host.
// For block mode, the supervisor mounts the pre-attached /dev/vdX drive.
func (c *VsockClient) MountVolume(req MountVolumeRequest) error {
	params, _ := json.Marshal(req)
	_, err := c.call("mount_volume", params)
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
	// Connect to the Firecracker vsock UDS and send CONNECT command.
	// Firecracker's vsock host-side protocol: connect to UDS, send "CONNECT <port>\n",
	// then receive "OK <port>\n" — after that it's a raw bidirectional stream.
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("vsock connect: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))

	// Firecracker vsock handshake.
	if _, err := fmt.Fprintf(conn, "CONNECT 52\n"); err != nil {
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	// Read "OK <port>\n" response.
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("vsock handshake read: %w", err)
	}
	resp := string(buf[:n])
	if len(resp) < 2 || resp[:2] != "OK" {
		return nil, fmt.Errorf("vsock handshake failed: %s", resp)
	}

	// Send request.
	req := RPCRequest{
		Method: method,
		ID:     uuid.New().String(),
		Params: params,
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("vsock write: %w", err)
	}

	// Read RPC response.
	var rpcResp RPCResponse
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("vsock read: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("supervisor error: %s", rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}
