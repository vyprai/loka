package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/worker/vm"
)

// Server is the supervisor process that runs inside the Firecracker VM.
// It listens on vsock port 52 and handles RPC calls from the worker on the host.
// This is the ONLY process that can spawn commands inside the VM.
type Server struct {
	executor *Executor
	listener net.Listener
	logger   *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	service  *ServiceProcess // At most one service per VM.
}

// NewServer creates a new supervisor server.
func NewServer(policy loka.ExecPolicy, mode loka.ExecMode, logger *slog.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		executor: NewExecutor(policy, mode, "/env", "/workspace", "local",
			func(pa *PendingApproval) {
				logger.Info("command awaiting approval",
					"command_id", pa.ID,
					"command", pa.Command.Command,
					"reason", pa.Reason,
				)
			},
		),
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
}

// ListenAndServe starts the vsock listener.
// In production, this listens on vsock CID=3 port=52.
// For local testing, it listens on a unix domain socket.
func (s *Server) ListenAndServe(listenAddr string) error {
	var err error
	if strings.HasPrefix(listenAddr, "vsock:") {
		// Listen on vsock inside the VM. Format: "vsock:52"
		var port uint32
		fmt.Sscanf(listenAddr, "vsock:%d", &port)
		s.listener, err = vsock.Listen(port, nil)
	} else {
		s.listener, err = net.Listen("unix", listenAddr)
	}
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	s.logger.Info("supervisor listening", "addr", listenAddr)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return nil
			default:
				s.logger.Error("accept error", "error", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

// Stop shuts down the server.
func (s *Server) Stop() {
	s.cancel()
	if s.listener != nil {
		s.listener.Close()
	}
	s.executor.CancelAll()
}

func (s *Server) handleConnection(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in connection handler", "panic", r)
		}
		conn.Close()
	}()

	// Read the raw request bytes instead of using json.NewDecoder to avoid
	// buffering past the JSON request boundary. This is critical for
	// tcp_forward: after the RPC response the connection becomes a raw TCP
	// tunnel, so any bytes consumed by a decoder's internal buffer would be
	// lost, causing "connection reset by peer".
	buf := make([]byte, 8192)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}

	var req vm.RPCRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		s.logger.Error("decode request", "error", err)
		errResp := vm.RPCResponse{Error: &vm.RPCError{Code: -1, Message: fmt.Sprintf("invalid request: %v", err)}}
		json.NewEncoder(conn).Encode(errResp)
		return
	}

	encoder := json.NewEncoder(conn)

	// tcp_forward is special: after the RPC response the connection becomes
	// a raw TCP tunnel, so we handle it here instead of in handleRPC.
	if req.Method == "tcp_forward" {
		s.handleTCPForward(conn, encoder, req)
		return
	}

	// Set deadline based on method: long-running operations get more time.
	switch req.Method {
	case "service_start", "exec":
		conn.SetDeadline(time.Now().Add(30 * time.Minute))
	default:
		conn.SetDeadline(time.Now().Add(5 * time.Minute))
	}

	resp := s.handleRPC(req)
	encoder.Encode(resp)
}

func (s *Server) handleRPC(req vm.RPCRequest) vm.RPCResponse {
	switch req.Method {
	case "ping":
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"pong"`)}

	case "exec":
		return s.handleExec(req)

	case "set_mode":
		var params struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
		}
		s.executor.SetMode(loka.ExecMode(params.Mode))
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "set_policy":
		var policy loka.ExecPolicy
		if err := json.Unmarshal(req.Params, &policy); err != nil {
			return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
		}
		s.executor.SetPolicy(policy)
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "approve":
		var params struct {
			CommandID      string `json:"command_id"`
			AddToWhitelist bool   `json:"add_to_whitelist"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
		}
		if err := s.executor.Gate().Approve(params.CommandID, params.AddToWhitelist); err != nil {
			return rpcError(req.ID, err)
		}
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "deny":
		var params struct {
			CommandID string `json:"command_id"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
		}
		if err := s.executor.Gate().Deny(params.CommandID, params.Reason); err != nil {
			return rpcError(req.ID, err)
		}
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "list_pending":
		pending := s.executor.Gate().ListPending()
		var infos []vm.PendingInfo
		for _, p := range pending {
			infos = append(infos, vm.PendingInfo{
				ID:      p.ID,
				Command: p.Command.Command,
				Reason:  p.Reason,
			})
		}
		result, _ := json.Marshal(infos)
		return vm.RPCResponse{ID: req.ID, Result: result}

	case "cancel":
		var params struct {
			CommandID string `json:"command_id"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
		}
		if params.CommandID != "" {
			s.executor.Cancel(params.CommandID)
		} else {
			s.executor.CancelAll()
		}
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "audit_log":
		log := s.executor.GetAuditLog()
		result, _ := json.Marshal(log)
		return vm.RPCResponse{ID: req.ID, Result: result}

	case "health_check":
		return s.handleHealthCheck(req)

	case "service_start":
		return s.handleServiceStart(req)

	case "service_stop":
		return s.handleServiceStop(req)

	case "service_status":
		return s.handleServiceStatus(req)

	case "service_logs":
		return s.handleServiceLogs(req)

	default:
		return vm.RPCResponse{
			ID:    req.ID,
			Error: &vm.RPCError{Code: -1, Message: fmt.Sprintf("unknown method: %s", req.Method)},
		}
	}
}

func (s *Server) handleExec(req vm.RPCRequest) vm.RPCResponse {
	var params vm.ExecRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
	}

	var results []loka.CommandResult
	var status string

	if params.Parallel && len(params.Commands) > 1 {
		cmdResults, verdict, err := s.executor.ExecuteParallel(s.ctx, params.Commands)
		if verdict == VerdictNeedsApproval {
			status = "pending_approval"
			result, _ := json.Marshal(vm.ExecResponse{Status: status, Error: err.Error()})
			return vm.RPCResponse{ID: req.ID, Result: result}
		}
		if err != nil {
			status = "failed"
			result, _ := json.Marshal(vm.ExecResponse{Status: status, Results: cmdResults, Error: err.Error()})
			return vm.RPCResponse{ID: req.ID, Result: result}
		}
		results = cmdResults
		status = "success"
	} else {
		for _, cmd := range params.Commands {
			execResult, err := s.executor.Execute(s.ctx, cmd)
			if err != nil {
				if execResult != nil && execResult.Verdict == VerdictNeedsApproval {
					status = "pending_approval"
					result, _ := json.Marshal(vm.ExecResponse{Status: status, Results: results, Error: execResult.Reason})
					return vm.RPCResponse{ID: req.ID, Result: result}
				}
				if execResult != nil && execResult.Result != nil {
					results = append(results, *execResult.Result)
				}
				status = "failed"
				result, _ := json.Marshal(vm.ExecResponse{Status: status, Results: results, Error: err.Error()})
				return vm.RPCResponse{ID: req.ID, Result: result}
			}
			results = append(results, *execResult.Result)
		}
		status = "success"
	}

	// Check for any non-zero exit codes.
	for _, r := range results {
		if r.ExitCode != 0 {
			status = "failed"
			break
		}
	}

	result, _ := json.Marshal(vm.ExecResponse{Status: status, Results: results})
	return vm.RPCResponse{ID: req.ID, Result: result}
}

// handleTCPForward connects to localhost:port inside the VM and relays data
// bidirectionally over the vsock connection. After the success response, the
// vsock connection becomes a raw TCP tunnel — no more JSON-RPC on it.
func (s *Server) handleTCPForward(conn net.Conn, encoder *json.Encoder, req vm.RPCRequest) {
	var params struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		encoder.Encode(rpcError(req.ID, fmt.Errorf("invalid params: %w", err)))
		return
	}
	if params.Port <= 0 || params.Port > 65535 {
		encoder.Encode(rpcError(req.ID, fmt.Errorf("invalid port: %d", params.Port)))
		return
	}

	// Connect to the target service running inside the VM.
	targetAddr := fmt.Sprintf("127.0.0.1:%d", params.Port)
	targetConn, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		encoder.Encode(rpcError(req.ID, fmt.Errorf("connect to %s: %w", targetAddr, err)))
		return
	}

	// Send success response. After this, the connection is a raw TCP tunnel.
	encoder.Encode(vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)})

	// Clear deadlines — the tunnel can live as long as the TCP connection.
	conn.SetDeadline(time.Time{})
	targetConn.SetDeadline(time.Time{})

	s.logger.Info("tcp_forward tunnel established", "port", params.Port)

	// Bidirectional relay.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(targetConn, conn)
		// When the vsock side closes, half-close the target side.
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn, targetConn)
		// When the target side closes, half-close the vsock side.
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	wg.Wait()

	targetConn.Close()
	// conn is closed by the deferred Close in handleConnection.
}

func jsonRaw(s string) json.RawMessage {
	return json.RawMessage(s)
}

func rpcError(id string, err error) vm.RPCResponse {
	return vm.RPCResponse{
		ID:    id,
		Error: &vm.RPCError{Code: -1, Message: err.Error()},
	}
}

// ── Health Check RPC Handler ─────────────────────────────

func (s *Server) handleHealthCheck(req vm.RPCRequest) vm.RPCResponse {
	var params struct {
		Port int    `json:"port"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
	}
	if params.Port <= 0 || params.Port > 65535 {
		return rpcError(req.ID, fmt.Errorf("invalid port: %d", params.Port))
	}

	// TCP connect check.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", params.Port), 3*time.Second)
	if err != nil {
		return rpcError(req.ID, fmt.Errorf("port %d not listening: %w", params.Port, err))
	}
	conn.Close()

	// If path provided, do HTTP GET.
	if params.Path != "" {
		resp, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d%s", params.Port, params.Path), 3*time.Second)
		if err != nil {
			return rpcError(req.ID, fmt.Errorf("health check HTTP GET failed: %w", err))
		}
		if resp >= 400 {
			return rpcError(req.ID, fmt.Errorf("health check HTTP GET returned status %d", resp))
		}
	}

	return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"healthy"`)}
}

// httpGet performs an HTTP GET with a timeout and returns the status code.
func httpGet(url string, timeout time.Duration) (int, error) {
	client := &httpClient{timeout: timeout}
	return client.get(url)
}

// httpClient is a minimal HTTP client for health checks inside the VM.
// We avoid importing net/http to keep the supervisor binary small, but
// for health checks we need it.
type httpClient struct {
	timeout time.Duration
}

func (c *httpClient) get(url string) (int, error) {
	// Parse the URL to extract host and path.
	// URL format: http://127.0.0.1:PORT/path
	conn, err := net.DialTimeout("tcp", extractHost(url), c.timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))

	path := extractPath(url)
	req := fmt.Sprintf("GET %s HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n", path)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, err
	}

	// Read the response status line.
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, err
	}
	// Parse "HTTP/1.x STATUS ..." from the response.
	resp := string(buf[:n])
	var statusCode int
	if _, err := fmt.Sscanf(resp, "HTTP/%s %d", new(string), &statusCode); err != nil {
		// Try alternate parse for HTTP/1.0 or HTTP/1.1
		for i := 0; i < len(resp)-3; i++ {
			if resp[i] == ' ' {
				fmt.Sscanf(resp[i+1:], "%d", &statusCode)
				break
			}
		}
		if statusCode == 0 {
			return 0, fmt.Errorf("could not parse HTTP response: %.80s", resp)
		}
	}
	return statusCode, nil
}

// extractHost returns "host:port" from "http://host:port/path".
func extractHost(url string) string {
	// Strip scheme.
	s := url
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}
	// Strip path.
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return s
}

// extractPath returns "/path" from "http://host:port/path".
func extractPath(url string) string {
	s := url
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[idx:]
	}
	return "/"
}

// ── Service RPC Handlers ────────────────────────────────

func (s *Server) handleServiceStart(req vm.RPCRequest) vm.RPCResponse {
	var params struct {
		Command       string            `json:"command"`
		Args          []string          `json:"args"`
		Env           map[string]string `json:"env"`
		Workdir       string            `json:"workdir"`
		RestartPolicy string            `json:"restart_policy"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
	}

	if s.service != nil {
		// Stop existing service first.
		s.service.Stop(syscall.SIGTERM, 10*time.Second)
	}

	if params.RestartPolicy == "" {
		params.RestartPolicy = "on-failure"
	}

	sp := NewServiceProcess(params.Command, params.Args, params.Env, params.Workdir, params.RestartPolicy)
	if err := sp.Start(s.ctx); err != nil {
		return rpcError(req.ID, fmt.Errorf("start service: %w", err))
	}

	s.service = sp
	s.logger.Info("service started",
		"command", params.Command,
		"pid", sp.pid,
		"restart_policy", params.RestartPolicy,
	)

	result, _ := json.Marshal(map[string]int{"pid": sp.pid})
	return vm.RPCResponse{ID: req.ID, Result: result}
}

func (s *Server) handleServiceStop(req vm.RPCRequest) vm.RPCResponse {
	if s.service == nil {
		return rpcError(req.ID, fmt.Errorf("no service running"))
	}

	var params struct {
		Signal  string `json:"signal"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
	}

	sig := syscall.SIGTERM
	if params.Signal != "" {
		switch strings.ToUpper(params.Signal) {
		case "SIGKILL", "KILL":
			sig = syscall.SIGKILL
		case "SIGINT", "INT":
			sig = syscall.SIGINT
		case "SIGHUP", "HUP":
			sig = syscall.SIGHUP
		}
	}

	timeout := 10 * time.Second
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
	}

	if err := s.service.Stop(sig, timeout); err != nil {
		return rpcError(req.ID, err)
	}

	s.service = nil
	s.logger.Info("service stopped")
	result, _ := json.Marshal(map[string]bool{"ok": true})
	return vm.RPCResponse{ID: req.ID, Result: result}
}

func (s *Server) handleServiceStatus(req vm.RPCRequest) vm.RPCResponse {
	if s.service == nil {
		result, _ := json.Marshal(ServiceProcessStatus{})
		return vm.RPCResponse{ID: req.ID, Result: result}
	}

	status := s.service.Status()
	result, _ := json.Marshal(status)
	return vm.RPCResponse{ID: req.ID, Result: result}
}

func (s *Server) handleServiceLogs(req vm.RPCRequest) vm.RPCResponse {
	if s.service == nil {
		return rpcError(req.ID, fmt.Errorf("no service running"))
	}

	var params struct {
		Lines  int  `json:"lines"`
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid params: %w", err))
	}

	lines := params.Lines
	if lines <= 0 {
		lines = 100
	}

	result, _ := json.Marshal(vm.ServiceLogsResult{
		Stdout: s.service.stdout.Lines(lines),
		Stderr: s.service.stderr.Lines(lines),
	})
	return vm.RPCResponse{ID: req.ID, Result: result}
}
