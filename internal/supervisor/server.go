package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
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
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req vm.RPCRequest
	if err := decoder.Decode(&req); err != nil {
		s.logger.Error("decode request", "error", err)
		return
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
		json.Unmarshal(req.Params, &params)
		s.executor.SetMode(loka.ExecMode(params.Mode))
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "set_policy":
		var policy loka.ExecPolicy
		json.Unmarshal(req.Params, &policy)
		s.executor.SetPolicy(policy)
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "approve":
		var params struct {
			CommandID      string `json:"command_id"`
			AddToWhitelist bool   `json:"add_to_whitelist"`
		}
		json.Unmarshal(req.Params, &params)
		if err := s.executor.Gate().Approve(params.CommandID, params.AddToWhitelist); err != nil {
			return rpcError(req.ID, err)
		}
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}

	case "deny":
		var params struct {
			CommandID string `json:"command_id"`
			Reason    string `json:"reason"`
		}
		json.Unmarshal(req.Params, &params)
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
		json.Unmarshal(req.Params, &params)
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
	json.Unmarshal(req.Params, &params)

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

func jsonRaw(s string) json.RawMessage {
	return json.RawMessage(s)
}

func rpcError(id string, err error) vm.RPCResponse {
	return vm.RPCResponse{
		ID:    id,
		Error: &vm.RPCError{Code: -1, Message: err.Error()},
	}
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
	json.Unmarshal(req.Params, &params)

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
	json.Unmarshal(req.Params, &params)

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
