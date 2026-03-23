package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/vyprai/loka/internal/loka"
)

// GRPCService implements the worker-facing gRPC service on the control plane.
type GRPCService struct {
	registry *Registry
	logger   *slog.Logger

	// Callbacks for session/exec state updates.
	onSessionStatus    func(ctx context.Context, workerID, sessionID string, st loka.SessionStatus, errMsg string)
	onExecComplete     func(ctx context.Context, report ExecReport)
	onCheckpointDone   func(ctx context.Context, report CheckpointDoneReport)
}

// ExecReport is reported by workers when command execution completes.
type ExecReport struct {
	WorkerID  string
	SessionID string
	ExecID    string
	Status    loka.ExecStatus
	Results   []loka.CommandResult
	Error     string
}

// CheckpointDoneReport is reported by workers when a checkpoint operation completes.
type CheckpointDoneReport struct {
	WorkerID     string
	SessionID    string
	CheckpointID string
	Success      bool
	Error        string
	OverlayKey   string
	VMStateKey   string
}

// NewGRPCService creates a new worker gRPC service.
func NewGRPCService(
	registry *Registry,
	logger *slog.Logger,
	onSessionStatus func(ctx context.Context, workerID, sessionID string, st loka.SessionStatus, errMsg string),
	onExecComplete func(ctx context.Context, report ExecReport),
	onCheckpointDone func(ctx context.Context, report CheckpointDoneReport),
) *GRPCService {
	return &GRPCService{
		registry:         registry,
		logger:           logger,
		onSessionStatus:  onSessionStatus,
		onExecComplete:   onExecComplete,
		onCheckpointDone: onCheckpointDone,
	}
}

// We define a simplified gRPC-like interface without protoc-generated code.
// This will be replaced with proper generated code when we add buf.

// RegisterWorker handles worker registration.
func (s *GRPCService) RegisterWorker(ctx context.Context, req *RegisterReq) (*RegisterResp, error) {
	if req.Labels == nil {
		req.Labels = make(map[string]string)
	}

	w, err := s.registry.Register(ctx,
		req.Hostname, req.IPAddress, req.Provider,
		req.Region, req.Zone, req.AgentVersion,
		req.Capacity, req.Labels, req.KVMAvailable,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register: %v", err)
	}

	return &RegisterResp{
		WorkerID:                w.ID,
		HeartbeatIntervalSeconds: 5,
	}, nil
}

// RegisterReq is the registration request from a worker.
type RegisterReq struct {
	Hostname     string
	IPAddress    string
	Provider     string
	Region       string
	Zone         string
	AgentVersion string
	Capacity     loka.ResourceCapacity
	Labels       map[string]string
	KVMAvailable bool
}

// RegisterResp is the registration response to a worker.
type RegisterResp struct {
	WorkerID                 string
	HeartbeatIntervalSeconds int
}

// HeartbeatStream simulates the bidirectional heartbeat stream.
// In real gRPC, this would be a stream RPC.
type HeartbeatStream struct {
	WorkerID string
	InCh     chan *loka.Heartbeat  // Worker -> CP
	OutCh    chan WorkerCommand     // CP -> Worker
}

// ReportSessionStatus handles session status reports from workers.
func (s *GRPCService) ReportSessionStatus(ctx context.Context, workerID, sessionID string, st loka.SessionStatus, errMsg string) {
	if s.onSessionStatus != nil {
		s.onSessionStatus(ctx, workerID, sessionID, st, errMsg)
	}
}

// ReportExecComplete handles execution completion reports from workers.
func (s *GRPCService) ReportExecComplete(ctx context.Context, report ExecReport) {
	if s.onExecComplete != nil {
		s.onExecComplete(ctx, report)
	}
}

// ReportCheckpointComplete handles checkpoint completion reports from workers.
func (s *GRPCService) ReportCheckpointComplete(ctx context.Context, report CheckpointDoneReport) {
	if s.onCheckpointDone != nil {
		s.onCheckpointDone(ctx, report)
	}
}

// Ensure proto and grpc imports are used.
var (
	_ proto.Message
	_ grpc.ServerStream
	_ io.Reader
	_ sync.Mutex
	_ fmt.Stringer
)
