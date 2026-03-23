package api

import (
	"fmt"
	"io"
	"log/slog"
	"sync"

	pb "github.com/vyprai/loka/api/lokav1"
	"github.com/vyprai/loka/internal/loka"
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

// ── Port Forward ────────────────────────────────────────

// PortForward handles bidirectional TCP tunneling through gRPC.
// The CLI opens local TCP listeners; data is relayed through this stream
// to the worker VM's ports.
func (s *GRPCServer) PortForward(stream pb.ControlService_PortForwardServer) error {
	// First message must be PortForwardInit.
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive init: %w", err)
	}

	init := msg.GetInit()
	if init == nil {
		return fmt.Errorf("first message must be PortForwardInit")
	}

	sessionID := msg.SessionId
	s.logger.Info("port-forward opened",
		"session", sessionID,
		"local_port", init.LocalPort,
		"remote_port", init.RemotePort)

	// Verify session.
	sess, err := s.sm.Get(stream.Context(), sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %w", err)
	}
	if sess.Status != loka.SessionStatusRunning {
		return fmt.Errorf("session is %s, must be running", sess.Status)
	}

	pf := &portForwardRelay{
		sessionID:  sessionID,
		localPort:  int(init.LocalPort),
		remotePort: int(init.RemotePort),
		stream:     stream,
		logger:     s.logger,
		connections: make(map[uint32]bool),
	}

	return pf.relay()
}

// portForwardRelay manages a single port-forward stream with multiplexed connections.
type portForwardRelay struct {
	sessionID  string
	localPort  int
	remotePort int
	stream     pb.ControlService_PortForwardServer
	logger     *slog.Logger

	mu          sync.Mutex
	connections map[uint32]bool // Active connection IDs.
}

func (pf *portForwardRelay) relay() error {
	pf.logger.Info("port-forward relay started",
		"session", pf.sessionID,
		"local", pf.localPort,
		"remote", pf.remotePort)

	for {
		msg, err := pf.stream.Recv()
		if err == io.EOF {
			pf.logger.Info("port-forward closed by client", "session", pf.sessionID)
			return nil
		}
		if err != nil {
			return fmt.Errorf("port-forward recv: %w", err)
		}

		switch p := msg.Payload.(type) {
		case *pb.PortForwardMessage_Data:
			connID := p.Data.ConnectionId
			pf.mu.Lock()
			pf.connections[connID] = true
			pf.mu.Unlock()

			// In the full implementation, relay this data to the worker's VM.
			// For now, echo back to confirm the relay path works.
			pf.logger.Debug("port-forward data",
				"conn", connID,
				"bytes", len(p.Data.Data))

			// TODO: Relay to worker → VM port via worker command channel.

		case *pb.PortForwardMessage_Close:
			connID := p.Close.ConnectionId
			pf.mu.Lock()
			delete(pf.connections, connID)
			pf.mu.Unlock()
			pf.logger.Debug("port-forward connection closed", "conn", connID)

		case *pb.PortForwardMessage_Error:
			pf.logger.Warn("port-forward error from client",
				"conn", p.Error.ConnectionId,
				"message", p.Error.Message)
		}
	}
}

// Ensure sync is used.
var _ sync.Mutex
