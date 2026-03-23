package api

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/vyprai/loka/api/lokav1"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// GRPCServer implements the ControlService gRPC server.
type GRPCServer struct {
	pb.UnimplementedControlServiceServer
	sm       *session.Manager
	registry *worker.Registry
	logger   *slog.Logger
}

// NewGRPCServer creates a gRPC server wrapping the existing service layer.
func NewGRPCServer(sm *session.Manager, registry *worker.Registry, logger *slog.Logger) *GRPCServer {
	return &GRPCServer{sm: sm, registry: registry, logger: logger}
}

// Register registers the gRPC service on the given server.
func (s *GRPCServer) Register(srv *grpc.Server) {
	pb.RegisterControlServiceServer(srv, s)
}

// ─── Sessions ──────────────────────────────────────────

func (s *GRPCServer) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
	sess, err := s.sm.Create(ctx, session.CreateOpts{
		Name:     req.Name,
		ImageRef: req.Snapshot,
		Mode:     pbModeToLoka(req.Mode),
		VCPUs:    int(req.Vcpus),
		MemoryMB: int(req.MemoryMb),
		Labels:   req.Labels,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return sessionToProto(sess), nil
}

func (s *GRPCServer) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.Session, error) {
	sess, err := s.sm.Get(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session not found: %v", err)
	}
	return sessionToProto(sess), nil
}

func (s *GRPCServer) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	sessions, err := s.sm.List(ctx, store.SessionFilter{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	resp := &pb.ListSessionsResponse{Total: int32(len(sessions))}
	for _, sess := range sessions {
		resp.Sessions = append(resp.Sessions, sessionToProto(sess))
	}
	return resp, nil
}

func (s *GRPCServer) DestroySession(ctx context.Context, req *pb.DestroySessionRequest) (*emptypb.Empty, error) {
	if err := s.sm.Destroy(ctx, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *GRPCServer) PauseSession(ctx context.Context, req *pb.PauseSessionRequest) (*pb.Session, error) {
	sess, err := s.sm.Pause(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return sessionToProto(sess), nil
}

func (s *GRPCServer) ResumeSession(ctx context.Context, req *pb.ResumeSessionRequest) (*pb.Session, error) {
	sess, err := s.sm.Resume(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return sessionToProto(sess), nil
}

func (s *GRPCServer) SetSessionMode(ctx context.Context, req *pb.SetSessionModeRequest) (*pb.Session, error) {
	sess, err := s.sm.SetMode(ctx, req.Id, pbModeToLoka(req.Mode))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return sessionToProto(sess), nil
}

// ─── Execution ─────────────────────────────────────────

func (s *GRPCServer) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.Execution, error) {
	var cmds []loka.Command
	for _, c := range req.Commands {
		cmds = append(cmds, loka.Command{
			ID:      c.Id,
			Command: c.Command,
			Args:    c.Args,
			Workdir: c.Workdir,
			Env:     c.Env,
		})
	}
	exec, err := s.sm.Exec(ctx, req.SessionId, cmds, req.Parallel)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return execToProto(exec), nil
}

func (s *GRPCServer) GetExecution(ctx context.Context, req *pb.GetExecutionRequest) (*pb.Execution, error) {
	exec, err := s.sm.GetExecution(ctx, req.ExecId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	return execToProto(exec), nil
}

// ─── Checkpoints ───────────────────────────────────────

func (s *GRPCServer) CreateCheckpoint(ctx context.Context, req *pb.CreateCheckpointRequest) (*pb.Checkpoint, error) {
	cpType := loka.CheckpointLight
	if req.Type == pb.CheckpointType_CHECKPOINT_TYPE_FULL {
		cpType = loka.CheckpointFull
	}
	cp, err := s.sm.CreateCheckpoint(ctx, req.SessionId, "", cpType, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return checkpointToProto(cp), nil
}

func (s *GRPCServer) RestoreCheckpoint(ctx context.Context, req *pb.RestoreCheckpointRequest) (*pb.Session, error) {
	if err := s.sm.RestoreCheckpoint(ctx, req.SessionId, req.CheckpointId); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	sess, err := s.sm.Get(ctx, req.SessionId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return sessionToProto(sess), nil
}

// ─── Workers ───────────────────────────────────────────

func (s *GRPCServer) ListWorkers(ctx context.Context, req *pb.ListWorkersRequest) (*pb.ListWorkersResponse, error) {
	workers := s.registry.List()
	resp := &pb.ListWorkersResponse{Total: int32(len(workers))}
	for _, wc := range workers {
		resp.Workers = append(resp.Workers, workerToProto(wc.Worker))
	}
	return resp, nil
}

func (s *GRPCServer) GetWorker(ctx context.Context, req *pb.GetWorkerRequest) (*pb.Worker, error) {
	wc, ok := s.registry.Get(req.Id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "worker %q not found", req.Id)
	}
	return workerToProto(wc.Worker), nil
}

// ─── Converters ────────────────────────────────────────

func sessionToProto(s *loka.Session) *pb.Session {
	return &pb.Session{
		Id:        s.ID,
		Name:      s.Name,
		Status:    lokaSessStatusToProto(s.Status),
		Mode:      lokaModeToProto(s.Mode),
		WorkerId:  s.WorkerID,
		Snapshot:  s.ImageRef,
		Vcpus:     int32(s.VCPUs),
		MemoryMb:  int32(s.MemoryMB),
		Labels:    s.Labels,
		CreatedAt: timestamppb.New(s.CreatedAt),
		UpdatedAt: timestamppb.New(s.UpdatedAt),
	}
}

func execToProto(e *loka.Execution) *pb.Execution {
	exec := &pb.Execution{
		Id:        e.ID,
		SessionId: e.SessionID,
		Status:    lokaExecStatusToProto(e.Status),
		Parallel:  e.Parallel,
		CreatedAt: timestamppb.New(e.CreatedAt),
		UpdatedAt: timestamppb.New(e.UpdatedAt),
	}
	for _, c := range e.Commands {
		exec.Commands = append(exec.Commands, &pb.Command{
			Id:      c.ID,
			Command: c.Command,
			Args:    c.Args,
			Workdir: c.Workdir,
			Env:     c.Env,
		})
	}
	for _, r := range e.Results {
		exec.Results = append(exec.Results, &pb.CommandResult{
			CommandId: r.CommandID,
			ExitCode:  int32(r.ExitCode),
			Stdout:    r.Stdout,
			Stderr:    r.Stderr,
		})
	}
	return exec
}

func checkpointToProto(cp *loka.Checkpoint) *pb.Checkpoint {
	t := pb.CheckpointType_CHECKPOINT_TYPE_LIGHT
	if cp.Type == loka.CheckpointFull {
		t = pb.CheckpointType_CHECKPOINT_TYPE_FULL
	}
	return &pb.Checkpoint{
		Id:        cp.ID,
		SessionId: cp.SessionID,
		ParentId:  cp.ParentID,
		Type:      t,
		Label:     cp.Label,
		CreatedAt: timestamppb.New(cp.CreatedAt),
	}
}

func workerToProto(w *loka.Worker) *pb.Worker {
	return &pb.Worker{
		Id:       w.ID,
		Hostname: w.Hostname,
		Provider: w.Provider,
		Region:   w.Region,
		Labels:   w.Labels,
		Capacity: &pb.ResourceCapacity{
			CpuCores: int32(w.Capacity.CPUCores),
			MemoryMb: int64(w.Capacity.MemoryMB),
			DiskMb:   int64(w.Capacity.DiskMB),
		},
		CreatedAt: timestamppb.New(w.CreatedAt),
		LastSeen:  timestamppb.New(w.LastSeen),
	}
}

func pbModeToLoka(m pb.ExecMode) loka.ExecMode {
	switch m {
	case pb.ExecMode_EXEC_MODE_EXPLORE:
		return loka.ModeExplore
	case pb.ExecMode_EXEC_MODE_EXECUTE:
		return loka.ModeExecute
	case pb.ExecMode_EXEC_MODE_ASK:
		return loka.ModeAsk
	default:
		return loka.ModeExplore
	}
}

func lokaModeToProto(m loka.ExecMode) pb.ExecMode {
	switch m {
	case loka.ModeExplore:
		return pb.ExecMode_EXEC_MODE_EXPLORE
	case loka.ModeExecute:
		return pb.ExecMode_EXEC_MODE_EXECUTE
	case loka.ModeAsk:
		return pb.ExecMode_EXEC_MODE_ASK
	default:
		return pb.ExecMode_EXEC_MODE_UNSPECIFIED
	}
}

func lokaSessStatusToProto(s loka.SessionStatus) pb.SessionStatus {
	switch s {
	case loka.SessionStatusCreating:
		return pb.SessionStatus_SESSION_STATUS_CREATING
	case loka.SessionStatusRunning:
		return pb.SessionStatus_SESSION_STATUS_RUNNING
	case loka.SessionStatusPaused:
		return pb.SessionStatus_SESSION_STATUS_PAUSED
	case loka.SessionStatusTerminating:
		return pb.SessionStatus_SESSION_STATUS_TERMINATING
	case loka.SessionStatusTerminated:
		return pb.SessionStatus_SESSION_STATUS_TERMINATED
	case loka.SessionStatusError:
		return pb.SessionStatus_SESSION_STATUS_ERROR
	default:
		return pb.SessionStatus_SESSION_STATUS_UNSPECIFIED
	}
}

func lokaExecStatusToProto(s loka.ExecStatus) pb.ExecStatus {
	switch s {
	case loka.ExecStatusPending:
		return pb.ExecStatus_EXEC_STATUS_PENDING
	case loka.ExecStatusRunning:
		return pb.ExecStatus_EXEC_STATUS_RUNNING
	case loka.ExecStatusSuccess:
		return pb.ExecStatus_EXEC_STATUS_SUCCESS
	case loka.ExecStatusFailed:
		return pb.ExecStatus_EXEC_STATUS_FAILED
	case loka.ExecStatusCanceled:
		return pb.ExecStatus_EXEC_STATUS_CANCELED
	default:
		return pb.ExecStatus_EXEC_STATUS_UNSPECIFIED
	}
}
