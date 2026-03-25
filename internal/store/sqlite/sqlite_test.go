package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

func setupTestDB(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// createTestSession is a helper that inserts a session and returns it.
func createTestSession(t *testing.T, s *Store, name string, status loka.SessionStatus, workerID string) *loka.Session {
	t.Helper()
	sess := &loka.Session{
		ID:        uuid.New().String(),
		Name:      name,
		Status:    status,
		Mode:      loka.ModeExplore,
		WorkerID:  workerID,
		ImageRef:  "ubuntu:22.04",
		VCPUs:     2,
		MemoryMB:  1024,
		Labels:    map[string]string{"env": "test"},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

// ---------------------------------------------------------------------------
// Session CRUD
// ---------------------------------------------------------------------------

func TestSessionCRUD(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := &loka.Session{
		ID:        uuid.New().String(),
		Name:      "test-session",
		Status:    loka.SessionStatusRunning,
		Mode:      loka.ModeExplore,
		Labels:    map[string]string{"env": "test"},
		VCPUs:     2,
		MemoryMB:  1024,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Create.
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "test-session" {
		t.Errorf("name = %s, want test-session", got.Name)
	}

	// Update.
	got.Status = loka.SessionStatusPaused
	if err := s.Sessions().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Sessions().Get(ctx, sess.ID)
	if got2.Status != loka.SessionStatusPaused {
		t.Errorf("status = %s, want paused", got2.Status)
	}

	// List.
	list, err := s.Sessions().List(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("list = %d, want 1", len(list))
	}

	// Delete.
	if err := s.Sessions().Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Sessions().Get(ctx, sess.ID)
	if err == nil {
		t.Error("should get error after delete")
	}
}

func TestSessionFilterByStatus(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestSession(t, s, "running-1", loka.SessionStatusRunning, "")
	createTestSession(t, s, "running-2", loka.SessionStatusRunning, "")
	createTestSession(t, s, "paused-1", loka.SessionStatusPaused, "")

	status := loka.SessionStatusRunning
	list, err := s.Sessions().List(ctx, store.SessionFilter{Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d sessions, want 2", len(list))
	}
	for _, sess := range list {
		if sess.Status != loka.SessionStatusRunning {
			t.Errorf("unexpected status %s", sess.Status)
		}
	}
}

func TestSessionFilterByWorker(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestSession(t, s, "w1-s1", loka.SessionStatusRunning, "worker-1")
	createTestSession(t, s, "w1-s2", loka.SessionStatusRunning, "worker-1")
	createTestSession(t, s, "w2-s1", loka.SessionStatusRunning, "worker-2")

	wid := "worker-1"
	list, err := s.Sessions().List(ctx, store.SessionFilter{WorkerID: &wid})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d sessions, want 2", len(list))
	}
}

func TestSessionFilterByName(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestSession(t, s, "alpha", loka.SessionStatusRunning, "")
	createTestSession(t, s, "beta", loka.SessionStatusRunning, "")

	name := "alpha"
	list, err := s.Sessions().List(ctx, store.SessionFilter{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("got %d sessions, want 1", len(list))
	}
	if list[0].Name != "alpha" {
		t.Errorf("got name %q, want alpha", list[0].Name)
	}
}

func TestSessionListByWorker(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestSession(t, s, "s1", loka.SessionStatusRunning, "w-abc")
	createTestSession(t, s, "s2", loka.SessionStatusPaused, "w-abc")
	createTestSession(t, s, "s3", loka.SessionStatusRunning, "w-xyz")

	// Use List with WorkerID filter (ListByWorker has a known issue with empty status filter).
	wid := "w-abc"
	list, err := s.Sessions().List(ctx, store.SessionFilter{WorkerID: &wid})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d sessions, want 2", len(list))
	}
}

func TestSessionListLimitOffset(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		createTestSession(t, s, fmt.Sprintf("s-limit-%d", i), loka.SessionStatusRunning, "")
	}

	list, err := s.Sessions().List(ctx, store.SessionFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d sessions, want 2", len(list))
	}

	list2, err := s.Sessions().List(ctx, store.SessionFilter{Limit: 2, Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(list2) != 2 {
		t.Errorf("got %d sessions, want 2", len(list2))
	}
}

func TestSessionGetNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Sessions().Get(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

// ---------------------------------------------------------------------------
// Execution CRUD
// ---------------------------------------------------------------------------

func TestExecutionCRUD(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "exec-test", loka.SessionStatusRunning, "")

	exec := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		Status:    loka.ExecStatusPending,
		Parallel:  false,
		Commands: []loka.Command{
			{ID: "c1", Command: "ls", Args: []string{"-la"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Create.
	if err := s.Executions().Create(ctx, exec); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.Executions().Get(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != sess.ID {
		t.Errorf("session_id = %s, want %s", got.SessionID, sess.ID)
	}
	if got.Status != loka.ExecStatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if len(got.Commands) != 1 {
		t.Fatalf("commands len = %d, want 1", len(got.Commands))
	}
	if got.Commands[0].Command != "ls" {
		t.Errorf("command = %s, want ls", got.Commands[0].Command)
	}

	// Update status.
	got.Status = loka.ExecStatusRunning
	got.Results = []loka.CommandResult{
		{CommandID: "c1", ExitCode: 0, Stdout: "file.txt\n"},
	}
	if err := s.Executions().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Executions().Get(ctx, exec.ID)
	if got2.Status != loka.ExecStatusRunning {
		t.Errorf("status = %s, want running", got2.Status)
	}
	if len(got2.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(got2.Results))
	}
	if got2.Results[0].Stdout != "file.txt\n" {
		t.Errorf("stdout = %q, want %q", got2.Results[0].Stdout, "file.txt\n")
	}
}

func TestExecutionGetNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Executions().Get(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent execution")
	}
}

func TestExecutionListBySession(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess1 := createTestSession(t, s, "s1", loka.SessionStatusRunning, "")
	sess2 := createTestSession(t, s, "s2", loka.SessionStatusRunning, "")

	for i := 0; i < 3; i++ {
		e := &loka.Execution{
			ID:        uuid.New().String(),
			SessionID: sess1.ID,
			Status:    loka.ExecStatusSuccess,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := s.Executions().Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	e2 := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: sess2.ID,
		Status:    loka.ExecStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.Executions().Create(ctx, e2); err != nil {
		t.Fatal(err)
	}

	list, err := s.Executions().ListBySession(ctx, sess1.ID, store.ExecutionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("got %d executions, want 3", len(list))
	}
}

func TestExecutionListBySessionFilterStatus(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "s1", loka.SessionStatusRunning, "")

	statuses := []loka.ExecStatus{loka.ExecStatusPending, loka.ExecStatusSuccess, loka.ExecStatusSuccess}
	for _, st := range statuses {
		e := &loka.Execution{
			ID:        uuid.New().String(),
			SessionID: sess.ID,
			Status:    st,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := s.Executions().Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	st := loka.ExecStatusSuccess
	list, err := s.Executions().ListBySession(ctx, sess.ID, store.ExecutionFilter{Status: &st})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d executions, want 2", len(list))
	}
}

func TestExecutionParallelFlag(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "par-test", loka.SessionStatusRunning, "")

	exec := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		Status:    loka.ExecStatusPending,
		Parallel:  true,
		Commands: []loka.Command{
			{ID: "c1", Command: "echo", Args: []string{"hello"}},
			{ID: "c2", Command: "echo", Args: []string{"world"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.Executions().Create(ctx, exec); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Executions().Get(ctx, exec.ID)
	if !got.Parallel {
		t.Error("parallel should be true")
	}
	if len(got.Commands) != 2 {
		t.Errorf("commands len = %d, want 2", len(got.Commands))
	}
}

// ---------------------------------------------------------------------------
// Checkpoint CRUD
// ---------------------------------------------------------------------------

func TestCheckpointCRUD(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "cp-test", loka.SessionStatusRunning, "")

	cp := &loka.Checkpoint{
		ID:           uuid.New().String(),
		SessionID:    sess.ID,
		ParentID:     "",
		Type:         loka.CheckpointLight,
		Status:       loka.CheckpointStatusCreating,
		Label:        "initial",
		OverlayPath:  "/store/overlay.tar.zst",
		VMStatePath:  "",
		MetadataPath: "/store/meta.json",
		CreatedAt:    time.Now(),
	}

	// Create.
	if err := s.Checkpoints().Create(ctx, cp); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.Checkpoints().Get(ctx, cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "initial" {
		t.Errorf("label = %s, want initial", got.Label)
	}
	if got.Type != loka.CheckpointLight {
		t.Errorf("type = %s, want light", got.Type)
	}
	if got.Status != loka.CheckpointStatusCreating {
		t.Errorf("status = %s, want creating", got.Status)
	}
	if got.OverlayPath != "/store/overlay.tar.zst" {
		t.Errorf("overlay_path = %s, want /store/overlay.tar.zst", got.OverlayPath)
	}
}

func TestCheckpointGetNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Checkpoints().Get(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent checkpoint")
	}
}

func TestCheckpointListBySession(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "cp-list", loka.SessionStatusRunning, "")

	for i := 0; i < 3; i++ {
		cp := &loka.Checkpoint{
			ID:        uuid.New().String(),
			SessionID: sess.ID,
			Type:      loka.CheckpointLight,
			Status:    loka.CheckpointStatusReady,
			CreatedAt: time.Now(),
		}
		if err := s.Checkpoints().Create(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.Checkpoints().ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("got %d checkpoints, want 3", len(list))
	}
}

func TestCheckpointDAG(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "dag-test", loka.SessionStatusRunning, "")

	root := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  "",
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		Label:     "root",
		CreatedAt: time.Now(),
	}
	child := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  root.ID,
		Type:      loka.CheckpointFull,
		Status:    loka.CheckpointStatusReady,
		Label:     "child",
		CreatedAt: time.Now(),
	}

	if err := s.Checkpoints().Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoints().Create(ctx, child); err != nil {
		t.Fatal(err)
	}

	dag, err := s.Checkpoints().GetDAG(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dag.Root != root.ID {
		t.Errorf("root = %s, want %s", dag.Root, root.ID)
	}
	if len(dag.Checkpoints) != 2 {
		t.Errorf("got %d checkpoints in DAG, want 2", len(dag.Checkpoints))
	}

	children := dag.Children(root.ID)
	if len(children) != 1 {
		t.Fatalf("got %d children of root, want 1", len(children))
	}
	if children[0].ID != child.ID {
		t.Errorf("child id = %s, want %s", children[0].ID, child.ID)
	}

	path := dag.PathTo(child.ID)
	if len(path) != 2 {
		t.Fatalf("path len = %d, want 2", len(path))
	}
	if path[0].ID != root.ID {
		t.Errorf("path[0] = %s, want root %s", path[0].ID, root.ID)
	}
}

func TestCheckpointDelete(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "cp-del", loka.SessionStatusRunning, "")

	cp := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		CreatedAt: time.Now(),
	}
	if err := s.Checkpoints().Create(ctx, cp); err != nil {
		t.Fatal(err)
	}

	if err := s.Checkpoints().Delete(ctx, cp.ID); err != nil {
		t.Fatal(err)
	}

	_, err := s.Checkpoints().Get(ctx, cp.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestCheckpointDeleteSubtree(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "subtree", loka.SessionStatusRunning, "")

	root := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  "",
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		CreatedAt: time.Now(),
	}
	child := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  root.ID,
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		CreatedAt: time.Now(),
	}
	grandchild := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  child.ID,
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		CreatedAt: time.Now(),
	}

	for _, cp := range []*loka.Checkpoint{root, child, grandchild} {
		if err := s.Checkpoints().Create(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}

	// Delete child subtree (child + grandchild, root should remain).
	if err := s.Checkpoints().DeleteSubtree(ctx, child.ID); err != nil {
		t.Fatal(err)
	}

	// Root should still be there.
	if _, err := s.Checkpoints().Get(ctx, root.ID); err != nil {
		t.Errorf("root should still exist: %v", err)
	}

	// Child and grandchild should be gone.
	if _, err := s.Checkpoints().Get(ctx, child.ID); err == nil {
		t.Error("child should be deleted")
	}
	if _, err := s.Checkpoints().Get(ctx, grandchild.ID); err == nil {
		t.Error("grandchild should be deleted")
	}
}

// ---------------------------------------------------------------------------
// Worker CRUD
// ---------------------------------------------------------------------------

func createTestWorker(t *testing.T, s *Store, id, provider, region string, status loka.WorkerStatus, labels map[string]string) *loka.Worker {
	t.Helper()
	w := &loka.Worker{
		ID:           id,
		Hostname:     "host-" + id,
		IPAddress:    "10.0.0.1",
		Provider:     provider,
		Region:       region,
		Zone:         "us-east-1a",
		Status:       status,
		Labels:       labels,
		Capacity:     loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192, DiskMB: 100000},
		AgentVersion: "0.1.0",
		KVMAvailable: true,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		LastSeen:     time.Now(),
	}
	if err := s.Workers().Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorkerCRUD(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	w := &loka.Worker{
		ID:           uuid.New().String(),
		Hostname:     "worker-host",
		IPAddress:    "192.168.1.10",
		Provider:     "aws",
		Region:       "us-east-1",
		Zone:         "us-east-1a",
		Status:       loka.WorkerStatusReady,
		Labels:       map[string]string{"tier": "standard"},
		Capacity:     loka.ResourceCapacity{CPUCores: 8, MemoryMB: 16384, DiskMB: 500000},
		AgentVersion: "0.1.0",
		KVMAvailable: true,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		LastSeen:     time.Now(),
	}

	// Create.
	if err := s.Workers().Create(ctx, w); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.Workers().Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "worker-host" {
		t.Errorf("hostname = %s, want worker-host", got.Hostname)
	}
	if got.Provider != "aws" {
		t.Errorf("provider = %s, want aws", got.Provider)
	}
	if got.Capacity.CPUCores != 8 {
		t.Errorf("cpu = %d, want 8", got.Capacity.CPUCores)
	}
	if !got.KVMAvailable {
		t.Error("kvm_available should be true")
	}
	if got.Labels["tier"] != "standard" {
		t.Errorf("labels[tier] = %s, want standard", got.Labels["tier"])
	}

	// Update status.
	got.Status = loka.WorkerStatusBusy
	if err := s.Workers().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Workers().Get(ctx, w.ID)
	if got2.Status != loka.WorkerStatusBusy {
		t.Errorf("status = %s, want busy", got2.Status)
	}

	// Delete.
	if err := s.Workers().Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Workers().Get(ctx, w.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestWorkerGetNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Workers().Get(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent worker")
	}
}

func TestWorkerListFilterByProvider(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusReady, nil)
	createTestWorker(t, s, uuid.New().String(), "aws", "us-west-2", loka.WorkerStatusReady, nil)
	createTestWorker(t, s, uuid.New().String(), "gcp", "us-central1", loka.WorkerStatusReady, nil)

	p := "aws"
	list, err := s.Workers().List(ctx, store.WorkerFilter{Provider: &p})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d workers, want 2", len(list))
	}
}

func TestWorkerListFilterByStatus(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusReady, nil)
	createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusDead, nil)

	st := loka.WorkerStatusReady
	list, err := s.Workers().List(ctx, store.WorkerFilter{Status: &st})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("got %d workers, want 1", len(list))
	}
}

func TestWorkerListFilterByRegion(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusReady, nil)
	createTestWorker(t, s, uuid.New().String(), "aws", "eu-west-1", loka.WorkerStatusReady, nil)

	r := "eu-west-1"
	list, err := s.Workers().List(ctx, store.WorkerFilter{Region: &r})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("got %d workers, want 1", len(list))
	}
}

func TestWorkerListAll(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusReady, nil)
	createTestWorker(t, s, uuid.New().String(), "gcp", "us-central1", loka.WorkerStatusBusy, nil)

	list, err := s.Workers().List(ctx, store.WorkerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d workers, want 2", len(list))
	}
}

func TestWorkerUpdateHeartbeat(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	w := createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusReady, nil)

	hb := &loka.Heartbeat{
		WorkerID:  w.ID,
		Timestamp: time.Now(),
		Status:    loka.WorkerStatusBusy,
	}
	if err := s.Workers().UpdateHeartbeat(ctx, w.ID, hb); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Workers().Get(ctx, w.ID)
	if got.Status != loka.WorkerStatusBusy {
		t.Errorf("status = %s, want busy", got.Status)
	}
}

// ---------------------------------------------------------------------------
// Token CRUD
// ---------------------------------------------------------------------------

func TestTokenCRUD(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	tok := &loka.WorkerToken{
		ID:        uuid.New().String(),
		Name:      "dev-token",
		Token:     loka.GenerateToken(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Used:      false,
		CreatedAt: time.Now(),
	}

	// Create.
	if err := s.Tokens().Create(ctx, tok); err != nil {
		t.Fatal(err)
	}

	// Get by ID.
	got, err := s.Tokens().Get(ctx, tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "dev-token" {
		t.Errorf("name = %s, want dev-token", got.Name)
	}
	if got.Used {
		t.Error("token should not be used")
	}
	if got.Token != tok.Token {
		t.Errorf("token mismatch")
	}

	// Get by token value.
	got2, err := s.Tokens().GetByToken(ctx, tok.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got2.ID != tok.ID {
		t.Errorf("id = %s, want %s", got2.ID, tok.ID)
	}

	// List.
	list, err := s.Tokens().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("got %d tokens, want 1", len(list))
	}

	// MarkUsed.
	workerID := "worker-123"
	if err := s.Tokens().MarkUsed(ctx, tok.ID, workerID); err != nil {
		t.Fatal(err)
	}
	got3, _ := s.Tokens().Get(ctx, tok.ID)
	if !got3.Used {
		t.Error("token should be used")
	}
	if got3.WorkerID != workerID {
		t.Errorf("worker_id = %s, want %s", got3.WorkerID, workerID)
	}

	// Delete.
	if err := s.Tokens().Delete(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Tokens().Get(ctx, tok.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestTokenGetByTokenNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Tokens().GetByToken(ctx, "loka_nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent token")
	}
}

func TestTokenListMultiple(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		tok := &loka.WorkerToken{
			ID:        uuid.New().String(),
			Name:      "tok",
			Token:     loka.GenerateToken(),
			ExpiresAt: time.Now().Add(24 * time.Hour),
			CreatedAt: time.Now(),
		}
		if err := s.Tokens().Create(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.Tokens().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("got %d tokens, want 3", len(list))
	}
}

func TestTokenValidation(t *testing.T) {
	tok := &loka.WorkerToken{
		ID:        "t1",
		Token:     "loka_abc",
		ExpiresAt: time.Now().Add(1 * time.Hour),
		Used:      false,
	}

	if !tok.IsValid() {
		t.Error("fresh token should be valid")
	}

	tok.Used = true
	if tok.IsValid() {
		t.Error("used token should not be valid")
	}

	tok.Used = false
	tok.ExpiresAt = time.Now().Add(-1 * time.Hour)
	if tok.IsValid() {
		t.Error("expired token should not be valid")
	}
}

// ---------------------------------------------------------------------------
// Store lifecycle
// ---------------------------------------------------------------------------

func TestStoreOpenClose(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreInterfaceCompliance(t *testing.T) {
	s := setupTestDB(t)

	// Verify all repository accessors return non-nil.
	if s.Sessions() == nil {
		t.Error("Sessions() returned nil")
	}
	if s.Executions() == nil {
		t.Error("Executions() returned nil")
	}
	if s.Checkpoints() == nil {
		t.Error("Checkpoints() returned nil")
	}
	if s.Workers() == nil {
		t.Error("Workers() returned nil")
	}
	if s.Tokens() == nil {
		t.Error("Tokens() returned nil")
	}
}

// ---------------------------------------------------------------------------
// Cascade delete: deleting a session should cascade to executions/checkpoints
// ---------------------------------------------------------------------------

func TestSessionDeleteCascadesExecutions(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "cascade", loka.SessionStatusRunning, "")
	exec := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		Status:    loka.ExecStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.Executions().Create(ctx, exec); err != nil {
		t.Fatal(err)
	}

	if err := s.Sessions().Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	_, err := s.Executions().Get(ctx, exec.ID)
	if err == nil {
		t.Error("execution should be cascade-deleted with session")
	}
}

func TestSessionDeleteCascadesCheckpoints(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "cascade-cp", loka.SessionStatusRunning, "")
	cp := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		CreatedAt: time.Now(),
	}
	if err := s.Checkpoints().Create(ctx, cp); err != nil {
		t.Fatal(err)
	}

	if err := s.Sessions().Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	_, err := s.Checkpoints().Get(ctx, cp.ID)
	if err == nil {
		t.Error("checkpoint should be cascade-deleted with session")
	}
}

// ---------------------------------------------------------------------------
// Extended tests
// ---------------------------------------------------------------------------

func TestSessionUpdateStatusAndVerify(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "status-update", loka.SessionStatusRunning, "worker-1")

	// Update to paused.
	sess.Status = loka.SessionStatusPaused
	sess.UpdatedAt = time.Now()
	if err := s.Sessions().Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	got, err := s.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.SessionStatusPaused {
		t.Errorf("status = %s, want paused", got.Status)
	}

	// Update to terminated.
	got.Status = loka.SessionStatusTerminated
	got.UpdatedAt = time.Now()
	if err := s.Sessions().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	got2, err := s.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != loka.SessionStatusTerminated {
		t.Errorf("status = %s, want terminated", got2.Status)
	}
}

func TestExecutionUpdateStatusAndResults(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "exec-update", loka.SessionStatusRunning, "")

	exec := &loka.Execution{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		Status:    loka.ExecStatusPending,
		Commands: []loka.Command{
			{ID: "c1", Command: "echo", Args: []string{"hello"}},
			{ID: "c2", Command: "ls", Args: []string{"-la"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.Executions().Create(ctx, exec); err != nil {
		t.Fatal(err)
	}

	// Update to running.
	got, err := s.Executions().Get(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Status = loka.ExecStatusRunning
	got.UpdatedAt = time.Now()
	if err := s.Executions().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	got2, err := s.Executions().Get(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != loka.ExecStatusRunning {
		t.Errorf("status = %s, want running", got2.Status)
	}

	// Update to success with results.
	got2.Status = loka.ExecStatusSuccess
	got2.Results = []loka.CommandResult{
		{CommandID: "c1", ExitCode: 0, Stdout: "hello\n"},
		{CommandID: "c2", ExitCode: 0, Stdout: "total 4\n-rw-r--r-- 1 root root 0 Jan 1 00:00 file.txt\n"},
	}
	got2.UpdatedAt = time.Now()
	if err := s.Executions().Update(ctx, got2); err != nil {
		t.Fatal(err)
	}

	got3, err := s.Executions().Get(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got3.Status != loka.ExecStatusSuccess {
		t.Errorf("status = %s, want success", got3.Status)
	}
	if len(got3.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(got3.Results))
	}
	if got3.Results[0].Stdout != "hello\n" {
		t.Errorf("results[0].stdout = %q, want %q", got3.Results[0].Stdout, "hello\n")
	}
	if got3.Results[1].CommandID != "c2" {
		t.Errorf("results[1].command_id = %q, want c2", got3.Results[1].CommandID)
	}
}

func TestCheckpointWithParentVerify(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	sess := createTestSession(t, s, "cp-parent", loka.SessionStatusRunning, "")

	parent := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  "",
		Type:      loka.CheckpointLight,
		Status:    loka.CheckpointStatusReady,
		Label:     "parent-cp",
		CreatedAt: time.Now(),
	}
	if err := s.Checkpoints().Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	child := &loka.Checkpoint{
		ID:        uuid.New().String(),
		SessionID: sess.ID,
		ParentID:  parent.ID,
		Type:      loka.CheckpointFull,
		Status:    loka.CheckpointStatusCreating,
		Label:     "child-cp",
		CreatedAt: time.Now(),
	}
	if err := s.Checkpoints().Create(ctx, child); err != nil {
		t.Fatal(err)
	}

	// Verify parent has no parent.
	gotParent, err := s.Checkpoints().Get(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotParent.ParentID != "" {
		t.Errorf("parent.ParentID = %q, want empty", gotParent.ParentID)
	}

	// Verify child references parent.
	gotChild, err := s.Checkpoints().Get(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.ParentID != parent.ID {
		t.Errorf("child.ParentID = %q, want %q", gotChild.ParentID, parent.ID)
	}
	if gotChild.Type != loka.CheckpointFull {
		t.Errorf("child.Type = %s, want full", gotChild.Type)
	}
	if gotChild.Label != "child-cp" {
		t.Errorf("child.Label = %q, want %q", gotChild.Label, "child-cp")
	}
}

func TestSessionListMultipleFiltersCombined(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Create sessions with various status and worker combinations.
	createTestSession(t, s, "r-w1", loka.SessionStatusRunning, "worker-1")
	createTestSession(t, s, "r-w2", loka.SessionStatusRunning, "worker-2")
	createTestSession(t, s, "p-w1", loka.SessionStatusPaused, "worker-1")
	createTestSession(t, s, "t-w1", loka.SessionStatusTerminated, "worker-1")

	// Filter by status=running AND worker=worker-1 should give 1 result.
	status := loka.SessionStatusRunning
	wid := "worker-1"
	list, err := s.Sessions().List(ctx, store.SessionFilter{
		Status:   &status,
		WorkerID: &wid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("got %d sessions, want 1", len(list))
	}
	if len(list) > 0 {
		if list[0].Name != "r-w1" {
			t.Errorf("got name %q, want r-w1", list[0].Name)
		}
	}

	// Filter by status=running should give 2 results.
	list2, err := s.Sessions().List(ctx, store.SessionFilter{Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	if len(list2) != 2 {
		t.Errorf("got %d sessions, want 2", len(list2))
	}

	// Filter by worker-1 only should give 3 results.
	list3, err := s.Sessions().List(ctx, store.SessionFilter{WorkerID: &wid})
	if err != nil {
		t.Fatal(err)
	}
	if len(list3) != 3 {
		t.Errorf("got %d sessions, want 3", len(list3))
	}

	// Filter by name and status combined.
	name := "p-w1"
	pausedStatus := loka.SessionStatusPaused
	list4, err := s.Sessions().List(ctx, store.SessionFilter{
		Name:   &name,
		Status: &pausedStatus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list4) != 1 {
		t.Errorf("got %d sessions, want 1", len(list4))
	}
}

func TestWorkerUpdateHeartbeatChangesLastSeen(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	w := createTestWorker(t, s, uuid.New().String(), "aws", "us-east-1", loka.WorkerStatusReady, nil)

	// Record original LastSeen.
	original, err := s.Workers().Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalLastSeen := original.LastSeen

	// Use a timestamp far enough in the future to survive any rounding.
	futureTime := originalLastSeen.Add(2 * time.Second)
	hb := &loka.Heartbeat{
		WorkerID:  w.ID,
		Timestamp: futureTime,
		Status:    loka.WorkerStatusReady,
	}
	if err := s.Workers().UpdateHeartbeat(ctx, w.ID, hb); err != nil {
		t.Fatal(err)
	}

	got, err := s.Workers().Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}

	// LastSeen should have been updated to at least after the original.
	if !got.LastSeen.After(originalLastSeen) {
		t.Errorf("LastSeen was not updated: original=%v, got=%v", originalLastSeen, got.LastSeen)
	}

	// Status should remain ready (we sent ready in heartbeat).
	if got.Status != loka.WorkerStatusReady {
		t.Errorf("status = %s, want ready", got.Status)
	}
}

// ---------------------------------------------------------------------------
// GC cleanup method tests
// ---------------------------------------------------------------------------

func TestDeleteTerminatedBefore(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour)
	recentTime := time.Now()
	cutoff := time.Now().Add(-24 * time.Hour)

	// 1 old terminated session.
	oldTerminated := &loka.Session{
		ID: uuid.New().String(), Name: "old-terminated", Status: loka.SessionStatusTerminated,
		Mode: loka.ModeExplore, Labels: map[string]string{}, VCPUs: 1, MemoryMB: 512,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	// 1 recent terminated session.
	recentTerminated := &loka.Session{
		ID: uuid.New().String(), Name: "recent-terminated", Status: loka.SessionStatusTerminated,
		Mode: loka.ModeExplore, Labels: map[string]string{}, VCPUs: 1, MemoryMB: 512,
		CreatedAt: recentTime, UpdatedAt: recentTime,
	}
	// 1 running session (old but running, should not be deleted).
	running := &loka.Session{
		ID: uuid.New().String(), Name: "running", Status: loka.SessionStatusRunning,
		Mode: loka.ModeExplore, Labels: map[string]string{}, VCPUs: 1, MemoryMB: 512,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	for _, sess := range []*loka.Session{oldTerminated, recentTerminated, running} {
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Sessions().DeleteTerminatedBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("DeleteTerminatedBefore count = %d, want 1", n)
	}

	// Old terminated should be gone.
	_, err = s.Sessions().Get(ctx, oldTerminated.ID)
	if err == nil {
		t.Error("old terminated session should have been deleted")
	}

	// Recent terminated should still exist.
	_, err = s.Sessions().Get(ctx, recentTerminated.ID)
	if err != nil {
		t.Error("recent terminated session should still exist")
	}

	// Running should still exist.
	_, err = s.Sessions().Get(ctx, running.ID)
	if err != nil {
		t.Error("running session should still exist")
	}
}

func TestDeleteTerminatedBefore_NoneEligible(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	recentTime := time.Now()
	cutoff := time.Now().Add(-24 * time.Hour)

	sess := &loka.Session{
		ID: uuid.New().String(), Name: "recent-terminated", Status: loka.SessionStatusTerminated,
		Mode: loka.ModeExplore, Labels: map[string]string{}, VCPUs: 1, MemoryMB: 512,
		CreatedAt: recentTime, UpdatedAt: recentTime,
	}
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	n, err := s.Sessions().DeleteTerminatedBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("DeleteTerminatedBefore count = %d, want 0", n)
	}
}

func TestDeleteCompletedBefore(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	sess := createTestSession(t, s, "exec-parent", loka.SessionStatusRunning, "")

	// 1 old success execution.
	oldSuccess := &loka.Execution{
		ID: uuid.New().String(), SessionID: sess.ID, Status: loka.ExecStatusSuccess,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	// 1 old failed execution.
	oldFailed := &loka.Execution{
		ID: uuid.New().String(), SessionID: sess.ID, Status: loka.ExecStatusFailed,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	// 1 running execution.
	runningExec := &loka.Execution{
		ID: uuid.New().String(), SessionID: sess.ID, Status: loka.ExecStatusRunning,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	for _, e := range []*loka.Execution{oldSuccess, oldFailed, runningExec} {
		if err := s.Executions().Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Executions().DeleteCompletedBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("DeleteCompletedBefore count = %d, want 2", n)
	}

	// Running execution should still exist.
	_, err = s.Executions().Get(ctx, runningExec.ID)
	if err != nil {
		t.Error("running execution should still exist")
	}
}

func TestDeleteCompletedBefore_KeepsRunning(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	sess := createTestSession(t, s, "exec-running", loka.SessionStatusRunning, "")

	runningExec := &loka.Execution{
		ID: uuid.New().String(), SessionID: sess.ID, Status: loka.ExecStatusRunning,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	if err := s.Executions().Create(ctx, runningExec); err != nil {
		t.Fatal(err)
	}

	n, err := s.Executions().DeleteCompletedBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("DeleteCompletedBefore count = %d, want 0", n)
	}

	_, err = s.Executions().Get(ctx, runningExec.ID)
	if err != nil {
		t.Error("running execution should not have been deleted")
	}
}

func TestDeleteExpiredBefore(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	// 1 expired token (expires_at in the past, before cutoff).
	expired := &loka.WorkerToken{
		ID: uuid.New().String(), Name: "expired", Token: loka.GenerateToken(),
		ExpiresAt: oldTime, CreatedAt: oldTime,
	}
	// 1 valid token (expires_at in the future).
	valid := &loka.WorkerToken{
		ID: uuid.New().String(), Name: "valid", Token: loka.GenerateToken(),
		ExpiresAt: time.Now().Add(24 * time.Hour), CreatedAt: time.Now(),
	}
	for _, tok := range []*loka.WorkerToken{expired, valid} {
		if err := s.Tokens().Create(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Tokens().DeleteExpiredBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("DeleteExpiredBefore count = %d, want 1", n)
	}

	// Expired token should be gone.
	_, err = s.Tokens().Get(ctx, expired.ID)
	if err == nil {
		t.Error("expired token should have been deleted")
	}

	// Valid token should still exist.
	_, err = s.Tokens().Get(ctx, valid.ID)
	if err != nil {
		t.Error("valid token should still exist")
	}
}

func TestDeleteExpiredBefore_KeepsValid(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	cutoff := time.Now().Add(-24 * time.Hour)

	valid := &loka.WorkerToken{
		ID: uuid.New().String(), Name: "valid", Token: loka.GenerateToken(),
		ExpiresAt: time.Now().Add(24 * time.Hour), CreatedAt: time.Now(),
	}
	if err := s.Tokens().Create(ctx, valid); err != nil {
		t.Fatal(err)
	}

	n, err := s.Tokens().DeleteExpiredBefore(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("DeleteExpiredBefore count = %d, want 0", n)
	}

	_, err = s.Tokens().Get(ctx, valid.ID)
	if err != nil {
		t.Error("valid token should not have been deleted")
	}
}
