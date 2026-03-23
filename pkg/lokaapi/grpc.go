package lokaapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vyprai/loka/api/lokav1"
)

// GRPCClient wraps a gRPC connection to the LOKA control plane.
type GRPCClient struct {
	conn   *grpc.ClientConn
	client pb.ControlServiceClient
}

// GRPCOpts configures the gRPC client.
type GRPCOpts struct {
	Address    string // host:port (gRPC port, default 6841)
	Token      string
	CACertPath string
	Insecure   bool // skip TLS verification
	PlainText  bool // no TLS at all (default for non-TLS servers)
}

// NewGRPCClient connects to the LOKA control plane via gRPC.
func NewGRPCClient(opts GRPCOpts) (*GRPCClient, error) {
	var dialOpts []grpc.DialOption

	if opts.PlainText {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else if opts.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true,
		})))
	} else if opts.CACertPath != "" {
		caCert, err := os.ReadFile(opts.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("invalid CA certificate")
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: pool,
		})))
	} else {
		// Default: plain text (no TLS).
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if opts.Token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(tokenAuth{token: opts.Token, plaintext: opts.PlainText}))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, opts.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("gRPC dial %s: %w", opts.Address, err)
	}

	return &GRPCClient{
		conn:   conn,
		client: pb.NewControlServiceClient(conn),
	}, nil
}

// Close closes the gRPC connection.
func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

// Proto returns the raw gRPC client for direct proto calls.
func (c *GRPCClient) Proto() pb.ControlServiceClient {
	return c.client
}

// ─── Session operations ────────────────────────────────

func (c *GRPCClient) CreateSession(ctx context.Context, req CreateSessionReq) (*Session, error) {
	resp, err := c.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name:     req.Name,
		Snapshot: req.Image,
		Mode:     lokaModeToPB(req.Mode),
		Vcpus:    int32(req.VCPUs),
		MemoryMb: int32(req.MemoryMB),
		Labels:   req.Labels,
	})
	if err != nil {
		return nil, err
	}
	return protoToSession(resp), nil
}

func (c *GRPCClient) GetSession(ctx context.Context, id string) (*Session, error) {
	resp, err := c.client.GetSession(ctx, &pb.GetSessionRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return protoToSession(resp), nil
}

func (c *GRPCClient) ListSessions(ctx context.Context) (*ListSessionsResp, error) {
	resp, err := c.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	result := &ListSessionsResp{Total: int(resp.Total)}
	for _, s := range resp.Sessions {
		result.Sessions = append(result.Sessions, *protoToSession(s))
	}
	return result, nil
}

func (c *GRPCClient) DestroySession(ctx context.Context, id string) error {
	_, err := c.client.DestroySession(ctx, &pb.DestroySessionRequest{Id: id})
	return err
}

func (c *GRPCClient) PauseSession(ctx context.Context, id string) (*Session, error) {
	resp, err := c.client.PauseSession(ctx, &pb.PauseSessionRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return protoToSession(resp), nil
}

func (c *GRPCClient) ResumeSession(ctx context.Context, id string) (*Session, error) {
	resp, err := c.client.ResumeSession(ctx, &pb.ResumeSessionRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return protoToSession(resp), nil
}

func (c *GRPCClient) SetSessionMode(ctx context.Context, id, mode string) (*Session, error) {
	resp, err := c.client.SetSessionMode(ctx, &pb.SetSessionModeRequest{
		Id:   id,
		Mode: lokaModeToPB(mode),
	})
	if err != nil {
		return nil, err
	}
	return protoToSession(resp), nil
}

// ─── Exec operations ───────────────────────────────────

func (c *GRPCClient) Exec(ctx context.Context, sessionID string, req ExecReq) (*Execution, error) {
	pbReq := &pb.ExecRequest{SessionId: sessionID, Parallel: req.Parallel}
	if req.Command != "" {
		pbReq.Commands = []*pb.Command{{
			Command: req.Command,
			Args:    req.Args,
			Workdir: req.Workdir,
			Env:     req.Env,
		}}
	}
	for _, cmd := range req.Commands {
		pbReq.Commands = append(pbReq.Commands, &pb.Command{
			Id:      cmd.ID,
			Command: cmd.Command,
			Args:    cmd.Args,
			Workdir: cmd.Workdir,
			Env:     cmd.Env,
		})
	}
	resp, err := c.client.Exec(ctx, pbReq)
	if err != nil {
		return nil, err
	}
	return protoToExecution(resp), nil
}

func (c *GRPCClient) GetExecution(ctx context.Context, sessionID, execID string) (*Execution, error) {
	resp, err := c.client.GetExecution(ctx, &pb.GetExecutionRequest{
		SessionId: sessionID,
		ExecId:    execID,
	})
	if err != nil {
		return nil, err
	}
	return protoToExecution(resp), nil
}

// ─── Checkpoint operations ─────────────────────────────

func (c *GRPCClient) CreateCheckpoint(ctx context.Context, sessionID string, req CreateCheckpointReq) (*Checkpoint, error) {
	cpType := pb.CheckpointType_CHECKPOINT_TYPE_LIGHT
	if req.Type == "full" {
		cpType = pb.CheckpointType_CHECKPOINT_TYPE_FULL
	}
	resp, err := c.client.CreateCheckpoint(ctx, &pb.CreateCheckpointRequest{
		SessionId: sessionID,
		Type:      cpType,
		Label:     req.Label,
	})
	if err != nil {
		return nil, err
	}
	return protoToCheckpoint(resp), nil
}

func (c *GRPCClient) ListCheckpoints(ctx context.Context, sessionID string) (*ListCheckpointsResp, error) {
	resp, err := c.client.ListCheckpoints(ctx, &pb.ListCheckpointsRequest{SessionId: sessionID})
	if err != nil {
		return nil, err
	}
	result := &ListCheckpointsResp{
		SessionID: resp.SessionId,
		Root:      resp.RootId,
		Current:   resp.CurrentId,
	}
	for _, cp := range resp.Checkpoints {
		result.Checkpoints = append(result.Checkpoints, *protoToCheckpoint(cp))
	}
	return result, nil
}

func (c *GRPCClient) RestoreCheckpoint(ctx context.Context, sessionID, cpID string) (*Session, error) {
	resp, err := c.client.RestoreCheckpoint(ctx, &pb.RestoreCheckpointRequest{
		SessionId:    sessionID,
		CheckpointId: cpID,
	})
	if err != nil {
		return nil, err
	}
	return protoToSession(resp), nil
}

// ─── Health (via gRPC reflection or unary) ─────────────

// Health checks connectivity by listing sessions (lightweight).
func (c *GRPCClient) Health(ctx context.Context) error {
	_, err := c.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	return err
}

// ─── Converters ────────────────────────────────────────

func protoToSession(s *pb.Session) *Session {
	sess := &Session{
		ID:       s.Id,
		Name:     s.Name,
		Status:   pbSessStatusToString(s.Status),
		Mode:     pbModeToString(s.Mode),
		WorkerID: s.WorkerId,
		ImageRef: s.Snapshot,
		VCPUs:    int(s.Vcpus),
		MemoryMB: int(s.MemoryMb),
		Labels:   s.Labels,
	}
	if s.CreatedAt != nil {
		sess.CreatedAt = s.CreatedAt.AsTime()
	}
	if s.UpdatedAt != nil {
		sess.UpdatedAt = s.UpdatedAt.AsTime()
	}
	return sess
}

func protoToExecution(e *pb.Execution) *Execution {
	exec := &Execution{
		ID:        e.Id,
		SessionID: e.SessionId,
		Status:    pbExecStatusToString(e.Status),
		Parallel:  e.Parallel,
	}
	if e.CreatedAt != nil {
		exec.CreatedAt = e.CreatedAt.AsTime()
	}
	if e.UpdatedAt != nil {
		exec.UpdatedAt = e.UpdatedAt.AsTime()
	}
	return exec
}

func protoToCheckpoint(cp *pb.Checkpoint) *Checkpoint {
	c := &Checkpoint{
		ID:        cp.Id,
		SessionID: cp.SessionId,
		ParentID:  cp.ParentId,
		Type:      "light",
		Label:     cp.Label,
	}
	if cp.Type == pb.CheckpointType_CHECKPOINT_TYPE_FULL {
		c.Type = "full"
	}
	if cp.CreatedAt != nil {
		c.CreatedAt = cp.CreatedAt.AsTime()
	}
	return c
}

func lokaModeToPB(mode string) pb.ExecMode {
	switch mode {
	case "explore":
		return pb.ExecMode_EXEC_MODE_EXPLORE
	case "execute":
		return pb.ExecMode_EXEC_MODE_EXECUTE
	case "ask":
		return pb.ExecMode_EXEC_MODE_ASK
	default:
		return pb.ExecMode_EXEC_MODE_UNSPECIFIED
	}
}

func pbModeToString(m pb.ExecMode) string {
	switch m {
	case pb.ExecMode_EXEC_MODE_EXPLORE:
		return "explore"
	case pb.ExecMode_EXEC_MODE_EXECUTE:
		return "execute"
	case pb.ExecMode_EXEC_MODE_ASK:
		return "ask"
	default:
		return "unknown"
	}
}

func pbSessStatusToString(s pb.SessionStatus) string {
	switch s {
	case pb.SessionStatus_SESSION_STATUS_CREATING:
		return "creating"
	case pb.SessionStatus_SESSION_STATUS_RUNNING:
		return "running"
	case pb.SessionStatus_SESSION_STATUS_PAUSED:
		return "paused"
	case pb.SessionStatus_SESSION_STATUS_TERMINATING:
		return "terminating"
	case pb.SessionStatus_SESSION_STATUS_TERMINATED:
		return "terminated"
	case pb.SessionStatus_SESSION_STATUS_ERROR:
		return "error"
	default:
		return "unknown"
	}
}

func pbExecStatusToString(s pb.ExecStatus) string {
	switch s {
	case pb.ExecStatus_EXEC_STATUS_PENDING:
		return "pending"
	case pb.ExecStatus_EXEC_STATUS_RUNNING:
		return "running"
	case pb.ExecStatus_EXEC_STATUS_SUCCESS:
		return "success"
	case pb.ExecStatus_EXEC_STATUS_FAILED:
		return "failed"
	case pb.ExecStatus_EXEC_STATUS_CANCELED:
		return "canceled"
	default:
		return "unknown"
	}
}

// tokenAuth implements grpc.PerRPCCredentials for bearer token auth.
type tokenAuth struct {
	token     string
	plaintext bool // When true, allows sending token over plaintext (insecure).
}

func (t tokenAuth) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

func (t tokenAuth) RequireTransportSecurity() bool {
	return !t.plaintext
}
