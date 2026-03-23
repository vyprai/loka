package api

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/vyprai/loka/api/lokav1"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store/sqlite"
)

// testEnv holds a gRPC server, client, and backing services for tests.
type testEnv struct {
	client   pb.ControlServiceClient
	sm       *session.Manager
	registry *worker.Registry
	conn     *grpc.ClientConn
	server   *grpc.Server
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	// Set up SQLite in-memory store.
	db, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	logger := slog.Default()
	registry := worker.NewRegistry(db, logger)
	sched := scheduler.New(registry, scheduler.StrategySpread)
	sm := session.NewManager(db, registry, sched, nil, logger)

	// Start gRPC server on a random port.
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	srv := grpc.NewServer()
	grpcSrv := NewGRPCServer(sm, registry, logger)
	grpcSrv.Register(srv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			// Server stopped — expected during cleanup.
		}
	}()

	// Create client connection.
	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		srv.Stop()
		t.Fatal(err)
	}

	client := pb.NewControlServiceClient(conn)

	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
		db.Close()
	})

	return &testEnv{
		client:   client,
		sm:       sm,
		registry: registry,
		conn:     conn,
		server:   srv,
	}
}

func TestCreateSession(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	resp, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name:     "test-session",
		Snapshot: "ubuntu:22.04",
		Mode:     pb.ExecMode_EXEC_MODE_EXPLORE,
		Vcpus:    2,
		MemoryMb: 1024,
		Labels:   map[string]string{"env": "test"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if resp.Id == "" {
		t.Error("expected non-empty session ID")
	}
	if resp.Name != "test-session" {
		t.Errorf("expected name %q, got %q", "test-session", resp.Name)
	}
	if resp.Snapshot != "ubuntu:22.04" {
		t.Errorf("expected snapshot %q, got %q", "ubuntu:22.04", resp.Snapshot)
	}
	if resp.Mode != pb.ExecMode_EXEC_MODE_EXPLORE {
		t.Errorf("expected mode EXPLORE, got %v", resp.Mode)
	}
	if resp.Vcpus != 2 {
		t.Errorf("expected vcpus 2, got %d", resp.Vcpus)
	}
	if resp.MemoryMb != 1024 {
		t.Errorf("expected memory_mb 1024, got %d", resp.MemoryMb)
	}
	if resp.Status != pb.SessionStatus_SESSION_STATUS_RUNNING {
		t.Errorf("expected status RUNNING, got %v", resp.Status)
	}
}

func TestCreateSessionDefaults(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	resp, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "defaults",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Manager defaults: mode=explore, vcpus=1, memory=512.
	if resp.Mode != pb.ExecMode_EXEC_MODE_EXPLORE {
		t.Errorf("expected default mode EXPLORE, got %v", resp.Mode)
	}
	if resp.Vcpus != 1 {
		t.Errorf("expected default vcpus 1, got %d", resp.Vcpus)
	}
	if resp.MemoryMb != 512 {
		t.Errorf("expected default memory 512, got %d", resp.MemoryMb)
	}
}

func TestGetSession(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "get-me",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Id != created.Id {
		t.Errorf("expected ID %q, got %q", created.Id, got.Id)
	}
	if got.Name != "get-me" {
		t.Errorf("expected name %q, got %q", "get-me", got.Name)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: "nonexistent-id"})
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %v", st.Code())
	}
}

func TestListSessions(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Empty list.
	resp, err := env.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("expected 0 sessions, got %d", resp.Total)
	}

	// Create two sessions.
	for _, name := range []string{"sess-a", "sess-b"} {
		_, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{Name: name})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", name, err)
		}
	}

	resp, err = env.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("expected 2 sessions, got %d", resp.Total)
	}
	if len(resp.Sessions) != 2 {
		t.Errorf("expected 2 session objects, got %d", len(resp.Sessions))
	}
}

func TestDestroySession(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "destroy-me",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err = env.client.DestroySession(ctx, &pb.DestroySessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	// Session should still be gettable, but with terminated status.
	got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("GetSession after destroy: %v", err)
	}
	if got.Status != pb.SessionStatus_SESSION_STATUS_TERMINATED {
		t.Errorf("expected status TERMINATED, got %v", got.Status)
	}
}

func TestSetSessionMode(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "mode-test",
		Mode: pb.ExecMode_EXEC_MODE_EXPLORE,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, err := env.client.SetSessionMode(ctx, &pb.SetSessionModeRequest{
		Id:   created.Id,
		Mode: pb.ExecMode_EXEC_MODE_EXECUTE,
	})
	if err != nil {
		t.Fatalf("SetSessionMode: %v", err)
	}
	if resp.Mode != pb.ExecMode_EXEC_MODE_EXECUTE {
		t.Errorf("expected mode EXECUTE, got %v", resp.Mode)
	}
}

func TestSetSessionModeInvalid(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.client.SetSessionMode(ctx, &pb.SetSessionModeRequest{
		Id:   "nonexistent",
		Mode: pb.ExecMode_EXEC_MODE_EXECUTE,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestExec(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Register a worker so exec can dispatch.
	_, err := env.registry.Register(ctx, "test-host", "127.0.0.1", "local", "us-east-1", "a", "1.0.0", defaultCapacity(), nil, true)
	if err != nil {
		t.Fatalf("Register worker: %v", err)
	}

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "exec-test",
		Mode: pb.ExecMode_EXEC_MODE_EXECUTE,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, err := env.client.Exec(ctx, &pb.ExecRequest{
		SessionId: created.Id,
		Commands: []*pb.Command{
			{
				Id:      "cmd-1",
				Command: "echo",
				Args:    []string{"hello"},
			},
		},
		Parallel: false,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Id == "" {
		t.Error("expected non-empty execution ID")
	}
	if resp.SessionId != created.Id {
		t.Errorf("expected session ID %q, got %q", created.Id, resp.SessionId)
	}
}

func TestExecNoWorker(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Create session without registering a worker (session runs without worker in dev mode).
	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "no-worker",
		Mode: pb.ExecMode_EXEC_MODE_EXECUTE,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err = env.client.Exec(ctx, &pb.ExecRequest{
		SessionId: created.Id,
		Commands: []*pb.Command{
			{Command: "echo", Args: []string{"test"}},
		},
	})
	// Should fail because no worker is assigned.
	if err == nil {
		t.Fatal("expected error when no worker is assigned")
	}
}

func TestListWorkers(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Initially empty.
	resp, err := env.client.ListWorkers(ctx, &pb.ListWorkersRequest{})
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("expected 0 workers, got %d", resp.Total)
	}

	// Register a worker.
	_, err = env.registry.Register(ctx, "worker-1", "10.0.0.1", "aws", "us-west-2", "a", "1.0.0", defaultCapacity(), map[string]string{"gpu": "true"}, true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	resp, err = env.client.ListWorkers(ctx, &pb.ListWorkersRequest{})
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected 1 worker, got %d", resp.Total)
	}
	if resp.Workers[0].Hostname != "worker-1" {
		t.Errorf("expected hostname %q, got %q", "worker-1", resp.Workers[0].Hostname)
	}
	if resp.Workers[0].Provider != "aws" {
		t.Errorf("expected provider %q, got %q", "aws", resp.Workers[0].Provider)
	}
}

func TestListWorkersMultiple(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	for i, name := range []string{"w1", "w2", "w3"} {
		_, err := env.registry.Register(ctx, name, "10.0.0."+string(rune('1'+i)), "local", "local", "", "1.0.0", defaultCapacity(), nil, false)
		if err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}

	resp, err := env.client.ListWorkers(ctx, &pb.ListWorkersRequest{})
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if resp.Total != 3 {
		t.Errorf("expected 3 workers, got %d", resp.Total)
	}
}

func TestDestroySessionNotFound(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.client.DestroySession(ctx, &pb.DestroySessionRequest{Id: "does-not-exist"})
	if err == nil {
		t.Fatal("expected error for destroying nonexistent session")
	}
}

func TestCreateAndListRoundTrip(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		_, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
			Name:     name,
			Snapshot: "python:3.12",
			Mode:     pb.ExecMode_EXEC_MODE_EXECUTE,
			Vcpus:    4,
			MemoryMb: 2048,
		})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", name, err)
		}
	}

	resp, err := env.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if int(resp.Total) != len(names) {
		t.Errorf("expected %d sessions, got %d", len(names), resp.Total)
	}

	// Verify each session can be individually retrieved.
	for _, s := range resp.Sessions {
		got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: s.Id})
		if err != nil {
			t.Errorf("GetSession(%s): %v", s.Id, err)
		}
		if got.Vcpus != 4 {
			t.Errorf("expected vcpus 4, got %d", got.Vcpus)
		}
	}
}

func TestSessionLabelsPreserved(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	labels := map[string]string{"team": "ml", "project": "loka"}
	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name:   "labels-test",
		Labels: labels,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	for k, v := range labels {
		if got.Labels[k] != v {
			t.Errorf("label %q: expected %q, got %q", k, v, got.Labels[k])
		}
	}
}

func TestGRPCPauseAndResume(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "pause-resume",
		Mode: pb.ExecMode_EXEC_MODE_EXPLORE,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.Status != pb.SessionStatus_SESSION_STATUS_RUNNING {
		t.Fatalf("expected initial status RUNNING, got %v", created.Status)
	}

	// Pause.
	paused, err := env.client.PauseSession(ctx, &pb.PauseSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("PauseSession: %v", err)
	}
	if paused.Status != pb.SessionStatus_SESSION_STATUS_PAUSED {
		t.Errorf("expected status PAUSED, got %v", paused.Status)
	}

	// Verify via Get.
	got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("GetSession after pause: %v", err)
	}
	if got.Status != pb.SessionStatus_SESSION_STATUS_PAUSED {
		t.Errorf("expected status PAUSED from Get, got %v", got.Status)
	}

	// Resume.
	resumed, err := env.client.ResumeSession(ctx, &pb.ResumeSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if resumed.Status != pb.SessionStatus_SESSION_STATUS_RUNNING {
		t.Errorf("expected status RUNNING after resume, got %v", resumed.Status)
	}

	// Verify via Get.
	got2, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("GetSession after resume: %v", err)
	}
	if got2.Status != pb.SessionStatus_SESSION_STATUS_RUNNING {
		t.Errorf("expected status RUNNING from Get, got %v", got2.Status)
	}
}

func TestGRPCSetModeTransitions(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "mode-transitions",
		Mode: pb.ExecMode_EXEC_MODE_EXPLORE,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	transitions := []pb.ExecMode{
		pb.ExecMode_EXEC_MODE_EXECUTE,
		pb.ExecMode_EXEC_MODE_ASK,
		pb.ExecMode_EXEC_MODE_EXPLORE,
	}

	for _, mode := range transitions {
		resp, err := env.client.SetSessionMode(ctx, &pb.SetSessionModeRequest{
			Id:   created.Id,
			Mode: mode,
		})
		if err != nil {
			t.Fatalf("SetSessionMode to %v: %v", mode, err)
		}
		if resp.Mode != mode {
			t.Errorf("expected mode %v, got %v", mode, resp.Mode)
		}

		// Verify via Get.
		got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
		if err != nil {
			t.Fatalf("GetSession after mode change: %v", err)
		}
		if got.Mode != mode {
			t.Errorf("Get: expected mode %v, got %v", mode, got.Mode)
		}
	}
}

func TestGRPCDestroyRunningSession(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	created, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
		Name: "destroy-running",
		Mode: pb.ExecMode_EXEC_MODE_EXECUTE,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.Status != pb.SessionStatus_SESSION_STATUS_RUNNING {
		t.Fatalf("expected status RUNNING, got %v", created.Status)
	}

	// Destroy the running session.
	_, err = env.client.DestroySession(ctx, &pb.DestroySessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	// Verify terminated.
	got, err := env.client.GetSession(ctx, &pb.GetSessionRequest{Id: created.Id})
	if err != nil {
		t.Fatalf("GetSession after destroy: %v", err)
	}
	if got.Status != pb.SessionStatus_SESSION_STATUS_TERMINATED {
		t.Errorf("expected TERMINATED, got %v", got.Status)
	}

	// List should still include it.
	list, err := env.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range list.Sessions {
		if s.Id == created.Id {
			found = true
			if s.Status != pb.SessionStatus_SESSION_STATUS_TERMINATED {
				t.Errorf("listed session status: expected TERMINATED, got %v", s.Status)
			}
		}
	}
	if !found {
		t.Error("destroyed session not found in list")
	}
}

func TestGRPCCreateMultipleSessions(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	ids := make(map[string]bool)
	for i := 0; i < 5; i++ {
		resp, err := env.client.CreateSession(ctx, &pb.CreateSessionRequest{
			Name:     "multi-" + string(rune('A'+i)),
			Snapshot: "ubuntu:22.04",
			Mode:     pb.ExecMode_EXEC_MODE_EXPLORE,
		})
		if err != nil {
			t.Fatalf("CreateSession %d: %v", i, err)
		}
		if ids[resp.Id] {
			t.Errorf("duplicate session ID: %s", resp.Id)
		}
		ids[resp.Id] = true
	}

	list, err := env.client.ListSessions(ctx, &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if list.Total != 5 {
		t.Errorf("expected 5 sessions, got %d", list.Total)
	}
	if len(list.Sessions) != 5 {
		t.Errorf("expected 5 session objects, got %d", len(list.Sessions))
	}

	// Verify each returned session has a unique ID that we created.
	for _, s := range list.Sessions {
		if !ids[s.Id] {
			t.Errorf("unexpected session ID in list: %s", s.Id)
		}
	}
}

// defaultCapacity returns a ResourceCapacity for test workers.
func defaultCapacity() loka.ResourceCapacity {
	return loka.ResourceCapacity{
		CPUCores: 8,
		MemoryMB: 16384,
		DiskMB:   102400,
	}
}
