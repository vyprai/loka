package vmagent

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"runtime"

	"github.com/mdlayher/vsock"
)

// VsockPort is the well-known vsock port that the agent listens on.
const VsockPort = 2222

// ExecRequest is a command execution request sent from the host.
type ExecRequest struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
}

// ExecResponse is the result of a command execution.
type ExecResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// Agent listens for exec requests and runs them inside the VM.
type Agent struct {
	listener net.Listener
}

// ListenVsock listens on vsock port 2222 for exec requests.
// On Linux with /dev/vsock available, it uses the vsock transport.
// Falls back to a Unix socket for testing on other platforms.
func ListenVsock() (*Agent, error) {
	if runtime.GOOS == "linux" {
		l, err := vsock.Listen(VsockPort, nil)
		if err == nil {
			return &Agent{listener: l}, nil
		}
		log.Printf("vsock listen failed, falling back to unix socket: %v", err)
	}

	// Fallback: unix socket for testing.
	socketPath := "/tmp/loka-vmagent.sock"
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	return &Agent{listener: l}, nil
}

// Serve accepts connections and handles exec requests.
// It blocks until the listener is closed.
func (a *Agent) Serve() error {
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go a.handleConnection(conn)
	}
}

// Close stops the agent listener.
func (a *Agent) Close() error {
	return a.listener.Close()
}

func (a *Agent) handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req ExecRequest
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(ExecResponse{ExitCode: -1, Error: err.Error()})
		return
	}

	cmd := exec.Command(req.Cmd, req.Args...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	}

	stdout, err := cmd.Output()
	resp := ExecResponse{
		Stdout: string(stdout),
	}

	if cmd.ProcessState != nil {
		resp.ExitCode = cmd.ProcessState.ExitCode()
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.Stderr = string(exitErr.Stderr)
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.Error = err.Error()
			resp.ExitCode = -1
		}
	}

	_ = encoder.Encode(resp)
}
