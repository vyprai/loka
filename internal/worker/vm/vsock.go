package vm

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

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
	json.Unmarshal(resp, &result)
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

// ── Low-Level Transport ─────────────────────────────────

func (c *VsockClient) call(method string, params json.RawMessage) (json.RawMessage, error) {
	// Connect to the vsock UDS.
	conn, err := net.DialTimeout("unix", c.socketPath+"_52", c.timeout)
	if err != nil {
		return nil, fmt.Errorf("vsock connect: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))

	// Send request.
	req := RPCRequest{
		Method: method,
		ID:     fmt.Sprintf("%d", time.Now().UnixNano()),
		Params: params,
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("vsock write: %w", err)
	}

	// Read response.
	var resp RPCResponse
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("vsock read: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("supervisor error: %s", resp.Error.Message)
	}

	return resp.Result, nil
}
