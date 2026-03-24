package supervisor

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/worker/vm"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func defaultTestPolicy() loka.ExecPolicy {
	return loka.DefaultExecPolicy()
}

func TestHandleRPC_MalformedParams(t *testing.T) {
	// Create a server with default policy.
	server := NewServer(defaultTestPolicy(), "execute", discardLogger())

	// Test all RPC methods that unmarshal params.
	methods := []string{
		"set_mode",
		"set_policy",
		"approve",
		"deny",
		"cancel",
		"exec",
		"service_stop",
		"service_logs",
	}

	malformedJSON := json.RawMessage(`{invalid json!!!`)

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := vm.RPCRequest{
				Method: method,
				ID:     "test-" + method,
				Params: malformedJSON,
			}
			resp := server.handleRPC(req)
			if resp.Error == nil {
				t.Errorf("handleRPC(%s) with malformed params: expected error response, got nil", method)
			}
		})
	}
}

func TestHandleRPC_ValidParams(t *testing.T) {
	server := NewServer(defaultTestPolicy(), "execute", discardLogger())

	// set_mode with valid params should succeed.
	params, _ := json.Marshal(map[string]string{"mode": "explore"})
	req := vm.RPCRequest{
		Method: "set_mode",
		ID:     "test-valid",
		Params: params,
	}
	resp := server.handleRPC(req)
	if resp.Error != nil {
		t.Errorf("handleRPC(set_mode) with valid params: unexpected error: %s", resp.Error.Message)
	}
}

func TestHandleRPC_ServiceStartMalformedParams(t *testing.T) {
	server := NewServer(defaultTestPolicy(), "execute", discardLogger())

	malformedJSON := json.RawMessage(`{invalid json!!!`)
	req := vm.RPCRequest{
		Method: "service_start",
		ID:     "test-service-start",
		Params: malformedJSON,
	}
	resp := server.handleRPC(req)
	if resp.Error == nil {
		t.Error("handleRPC(service_start) with malformed params: expected error, got nil")
	}
}
