package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	pb "github.com/vyprai/loka/api/lokav1"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// FileTunnel handles the bidirectional stream for mounting local files into a session.
// The CLI sends an init message, then the CP relays filesystem operations between
// the worker's VM and the CLI's local filesystem.
func (s *GRPCServer) FileTunnel(stream pb.ControlService_FileTunnelServer) error {
	// First message must be TunnelInit.
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive init: %w", err)
	}

	init := msg.GetInit()
	if init == nil {
		return fmt.Errorf("first message must be TunnelInit")
	}

	sessionID := msg.SessionId
	s.logger.Info("file tunnel opened",
		"session", sessionID,
		"local_path", init.LocalPath,
		"mount_path", init.MountPath,
		"read_only", init.ReadOnly)

	// Verify session exists and is running.
	sess, err := s.sm.Get(stream.Context(), sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %w", err)
	}
	if sess.Status != loka.SessionStatusRunning {
		return fmt.Errorf("session is %s, must be running", sess.Status)
	}

	// Register this tunnel so the worker can route filesystem requests through it.
	tunnel := &activeTunnel{
		sessionID: sessionID,
		mountPath: init.MountPath,
		localPath: init.LocalPath,
		readOnly:  init.ReadOnly,
		stream:    stream,
		logger:    s.logger,
	}

	// Relay messages between the worker and CLI until the stream closes.
	return tunnel.relay()
}

// activeTunnel manages a single file tunnel session.
type activeTunnel struct {
	sessionID string
	mountPath string
	localPath string
	readOnly  bool
	stream    pb.ControlService_FileTunnelServer
	logger    *slog.Logger
}

// relay reads messages from the stream and handles them.
// In the full implementation, this would relay between the CLI and the worker's VM.
// For now, it keeps the stream open and logs activity.
func (t *activeTunnel) relay() error {
	t.logger.Info("tunnel relay started",
		"session", t.sessionID,
		"mount", t.mountPath)

	for {
		msg, err := t.stream.Recv()
		if err == io.EOF {
			t.logger.Info("tunnel closed by client", "session", t.sessionID)
			return nil
		}
		if err != nil {
			return fmt.Errorf("tunnel recv: %w", err)
		}

		// Handle messages from the CLI side.
		switch p := msg.Payload.(type) {
		case *pb.FileTunnelMessage_ReadResp:
			t.logger.Debug("tunnel: read response", "eof", p.ReadResp.Eof, "bytes", len(p.ReadResp.Data))
		case *pb.FileTunnelMessage_WriteResp:
			t.logger.Debug("tunnel: write response", "bytes", p.WriteResp.BytesWritten)
		case *pb.FileTunnelMessage_ListResp:
			t.logger.Debug("tunnel: list response", "entries", len(p.ListResp.Entries))
		case *pb.FileTunnelMessage_StatResp:
			t.logger.Debug("tunnel: stat response", "exists", p.StatResp.Exists)
		case *pb.FileTunnelMessage_Error:
			t.logger.Warn("tunnel: error from client", "message", p.Error.Message, "path", p.Error.Path)
		default:
			t.logger.Debug("tunnel: unknown message type")
		}
	}
}

// PortForward is not implemented — port forwarding uses HTTP proxy only.
// Use `loka service port-forward` or domain proxy for port access.

// ── Interactive Shell ────────────────────────────────────

// Shell handles an interactive terminal session inside a VM.
// It dispatches a shell_start command to the worker, which opens a PTY in the
// VM via the supervisor's vsock connection. stdin/stdout/resize are relayed
// between the gRPC stream and the worker's PTY tunnel.
func (s *GRPCServer) Shell(stream pb.ControlService_ShellServer) error {
	// First message must be ShellInit.
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive init: %w", err)
	}

	init := msg.GetInit()
	if init == nil {
		return fmt.Errorf("first message must be ShellInit")
	}

	sessionID := msg.SessionId
	s.logger.Info("shell opened",
		"session", sessionID,
		"command", init.Command,
		"rows", init.Rows,
		"cols", init.Cols)

	// Resolve session ID (may be a name or short-ID).
	resolvedID, err := s.grpcResolveSessionID(stream.Context(), sessionID)
	if err != nil {
		s.logger.Error("shell: resolve session failed", "input", sessionID, "error", err)
		return fmt.Errorf("session not found: %w", err)
	}
	s.logger.Info("shell: resolved session", "input", sessionID, "resolved", resolvedID)

	// Verify session.
	sess, err := s.sm.Get(stream.Context(), resolvedID)
	if err != nil {
		s.logger.Error("shell: get session failed", "id", resolvedID, "error", err)
		return fmt.Errorf("session not found: %w", err)
	}
	s.logger.Info("shell: session found", "id", resolvedID, "status", sess.Status, "worker", sess.WorkerID)

	// Auto-wake idle sessions.
	if sess.Status == loka.SessionStatusIdle {
		_, err = s.sm.Wake(stream.Context(), resolvedID)
		if err != nil {
			return fmt.Errorf("wake session: %w", err)
		}
	} else if sess.Status != loka.SessionStatusRunning {
		return fmt.Errorf("session is %s, must be running", sess.Status)
	}

	if sess.WorkerID == "" {
		return fmt.Errorf("session has no worker assigned")
	}

	// Create relay and dispatch shell_start to the worker.
	relay := worker.NewShellRelay()
	env := make(map[string]string)
	if init.Env != nil {
		env = init.Env
	}

	s.logger.Info("shell: dispatching shell_start", "session", resolvedID, "worker", sess.WorkerID)
	if err := s.registry.SendCommand(sess.WorkerID, worker.WorkerCommand{
		ID:   "shell-" + resolvedID,
		Type: "shell_start",
		Data: worker.ShellStartData{
			SessionID: resolvedID,
			Command:   init.Command,
			Rows:      uint16(init.Rows),
			Cols:      uint16(init.Cols),
			Workdir:   init.Workdir,
			Env:       env,
			Relay:     relay,
		},
	}); err != nil {
		s.logger.Error("shell: SendCommand failed", "error", err)
		return fmt.Errorf("dispatch shell_start: %w", err)
	}
	s.logger.Info("shell: command dispatched, waiting for worker setup")

	// Wait for the worker to set up the PTY.
	if err := <-relay.ErrCh; err != nil {
		s.logger.Error("shell: worker setup failed", "error", err)
		return fmt.Errorf("shell setup: %w", err)
	}

	s.logger.Info("shell relay started", "session", resolvedID)

	// Goroutine: read from relay output → send to gRPC stream.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range relay.Output {
			switch f.Type {
			case worker.FrameData:
				stream.Send(&pb.ShellMessage{
					SessionId: resolvedID,
					Payload: &pb.ShellMessage_Output{
						Output: &pb.ShellOutput{Data: f.Data},
					},
				})
			case worker.FrameExit:
				exitCode := int32(0)
				if len(f.Data) >= 4 {
					exitCode = int32(f.Data[0])<<24 | int32(f.Data[1])<<16 | int32(f.Data[2])<<8 | int32(f.Data[3])
				}
				stream.Send(&pb.ShellMessage{
					SessionId: resolvedID,
					Payload: &pb.ShellMessage_Exit{
						Exit: &pb.ShellExit{ExitCode: exitCode},
					},
				})
				return
			}
		}
	}()

	// Main loop: read gRPC stream → write to relay input.
	go func() {
		defer close(relay.Input)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}

			switch p := msg.Payload.(type) {
			case *pb.ShellMessage_Input:
				select {
				case relay.Input <- worker.ShellFrame{Type: worker.FrameData, Data: p.Input.Data}:
				case <-done:
					return
				}
			case *pb.ShellMessage_Resize:
				payload := make([]byte, 4)
				payload[0] = byte(p.Resize.Rows >> 8)
				payload[1] = byte(p.Resize.Rows)
				payload[2] = byte(p.Resize.Cols >> 8)
				payload[3] = byte(p.Resize.Cols)
				select {
				case relay.Input <- worker.ShellFrame{Type: worker.FrameResize, Data: payload}:
				case <-done:
					return
				}
			}
		}
	}()

	// Wait for the shell to exit.
	<-done
	s.logger.Info("shell closed", "session", resolvedID)
	return nil
}

// grpcResolveSessionID resolves a session ID, name, or short-ID to a UUID.
func (s *GRPCServer) grpcResolveSessionID(ctx context.Context, idOrName string) (string, error) {
	// Full UUID — use directly.
	if _, err := uuid.Parse(idOrName); err == nil {
		return idOrName, nil
	}
	// Try direct ID lookup (non-UUID test IDs, etc.).
	if sess, err := s.sm.Get(ctx, idOrName); err == nil && sess != nil {
		return sess.ID, nil
	}
	// Try name lookup.
	sessions, err := s.sm.List(ctx, store.SessionFilter{Name: &idOrName, Limit: 1})
	if err == nil && len(sessions) > 0 {
		return sessions[0].ID, nil
	}
	return "", fmt.Errorf("session %q not found", idOrName)
}
