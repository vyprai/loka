package session

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/internal/store/sqlite"
)

// testEnv holds all dependencies for session Manager tests.
type testEnv struct {
	store    store.Store
	registry *worker.Registry
	sched    *scheduler.Scheduler
	manager  *Manager
	workerID string
}

// setupTestManager creates a Manager backed by an in-memory SQLite store,
// a worker registry with one registered worker, and a spread scheduler.
// The image manager is nil since image resolution is optional.
func setupTestManager(t *testing.T) *testEnv {
	t.Helper()

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// In-memory SQLite.
	st, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := worker.NewRegistry(st, logger)

	// Register a mock worker so the scheduler can pick it.
	w, err := reg.Register(ctx,
		"test-host",     // hostname
		"127.0.0.1",     // ipAddr
		"local",         // provider
		"us-east-1",     // region
		"us-east-1a",    // zone
		"1.0.0",         // agentVersion
		loka.ResourceCapacity{CPUCores: 4, MemoryMB: 8192, DiskMB: 50000},
		map[string]string{"env": "test"},
		true, // kvmAvailable
	)
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	sched := scheduler.New(reg, scheduler.StrategySpread)

	mgr := NewManager(st, reg, sched, nil, logger)

	return &testEnv{
		store:    st,
		registry: reg,
		sched:    sched,
		manager:  mgr,
		workerID: w.ID,
	}
}

// drainWorkerCommands reads and discards all pending commands on the worker channel.
func (te *testEnv) drainWorkerCommands(t *testing.T) []worker.WorkerCommand {
	t.Helper()
	conn, ok := te.registry.Get(te.workerID)
	if !ok {
		t.Fatal("worker not found in registry")
	}
	var cmds []worker.WorkerCommand
	for {
		select {
		case cmd := <-conn.CmdChan:
			cmds = append(cmds, cmd)
		default:
			return cmds
		}
	}
}

// createRunningSession is a helper that creates a session in running state.
func (te *testEnv) createRunningSession(t *testing.T, opts CreateOpts) *loka.Session {
	t.Helper()
	s, err := te.manager.Create(context.Background(), opts)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	te.drainWorkerCommands(t) // discard the launch command
	return s
}

// ─── Session lifecycle tests ─────────────────────────────────────

func TestCreateSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s, err := te.manager.Create(ctx, CreateOpts{
		Name:     "my-session",
		ImageRef: "ubuntu:22.04",
		Mode:     loka.ModeExplore,
		VCPUs:    2,
		MemoryMB: 1024,
		Labels:   map[string]string{"project": "test"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if s.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if s.Name != "my-session" {
		t.Errorf("Name = %q, want %q", s.Name, "my-session")
	}
	if s.Status != loka.SessionStatusRunning {
		t.Errorf("Status = %q, want %q", s.Status, loka.SessionStatusRunning)
	}
	if s.Mode != loka.ModeExplore {
		t.Errorf("Mode = %q, want %q", s.Mode, loka.ModeExplore)
	}
	if s.WorkerID != te.workerID {
		t.Errorf("WorkerID = %q, want %q", s.WorkerID, te.workerID)
	}
	if s.VCPUs != 2 {
		t.Errorf("VCPUs = %d, want 2", s.VCPUs)
	}
	if s.MemoryMB != 1024 {
		t.Errorf("MemoryMB = %d, want 1024", s.MemoryMB)
	}

	// Verify a launch_session command was sent.
	cmds := te.drainWorkerCommands(t)
	if len(cmds) == 0 {
		t.Fatal("expected launch_session command to be sent to worker")
	}
	if cmds[0].Type != "launch_session" {
		t.Errorf("command type = %q, want %q", cmds[0].Type, "launch_session")
	}
}

func TestCreateSessionDefaults(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s, err := te.manager.Create(ctx, CreateOpts{Name: "defaults-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	te.drainWorkerCommands(t)

	if s.Mode != loka.ModeExplore {
		t.Errorf("default Mode = %q, want %q", s.Mode, loka.ModeExplore)
	}
	if s.VCPUs != 1 {
		t.Errorf("default VCPUs = %d, want 1", s.VCPUs)
	}
	if s.MemoryMB != 512 {
		t.Errorf("default MemoryMB = %d, want 512", s.MemoryMB)
	}
}

func TestGetSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	created := te.createRunningSession(t, CreateOpts{Name: "get-test"})

	got, err := te.manager.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Name != "get-test" {
		t.Errorf("Name = %q, want %q", got.Name, "get-test")
	}
}

func TestGetSessionNotFound(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	_, err := te.manager.Get(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
}

func TestListSessions(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	te.createRunningSession(t, CreateOpts{Name: "list-1"})
	te.createRunningSession(t, CreateOpts{Name: "list-2"})
	te.createRunningSession(t, CreateOpts{Name: "list-3"})

	sessions, err := te.manager.List(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("len(sessions) = %d, want 3", len(sessions))
	}
}

func TestListSessionsWithStatusFilter(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "to-pause"})
	te.createRunningSession(t, CreateOpts{Name: "stay-running"})

	_, err := te.manager.Pause(ctx, s.ID)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	paused := loka.SessionStatusPaused
	sessions, err := te.manager.List(ctx, store.SessionFilter{Status: &paused})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("len(sessions) = %d, want 1", len(sessions))
	}
	if len(sessions) > 0 && sessions[0].ID != s.ID {
		t.Errorf("session ID = %q, want %q", sessions[0].ID, s.ID)
	}
}

func TestPauseSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "pause-test"})

	paused, err := te.manager.Pause(ctx, s.ID)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if paused.Status != loka.SessionStatusPaused {
		t.Errorf("Status = %q, want %q", paused.Status, loka.SessionStatusPaused)
	}
}

func TestPauseAlreadyPausedSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "double-pause"})

	_, err := te.manager.Pause(ctx, s.ID)
	if err != nil {
		t.Fatalf("first Pause: %v", err)
	}

	// Pausing again should fail since paused->paused is not a valid transition.
	_, err = te.manager.Pause(ctx, s.ID)
	if err == nil {
		t.Fatal("expected error when pausing already paused session")
	}
}

func TestResumeSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "resume-test"})

	_, err := te.manager.Pause(ctx, s.ID)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	resumed, err := te.manager.Resume(ctx, s.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != loka.SessionStatusRunning {
		t.Errorf("Status = %q, want %q", resumed.Status, loka.SessionStatusRunning)
	}
}

func TestResumeNonPausedSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "resume-fail"})

	// Resuming a running session should fail (running->running is not a valid transition via CanTransitionTo).
	// Actually running->running: ValidSessionTransitions[running] = [paused, terminating, error].
	// Running is NOT in that list, so CanTransitionTo(running) returns false.
	_, err := te.manager.Resume(ctx, s.ID)
	if err == nil {
		t.Fatal("expected error when resuming non-paused session")
	}
}

func TestDestroySession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "destroy-test"})

	err := te.manager.Destroy(ctx, s.ID)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := te.manager.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get after destroy: %v", err)
	}
	if got.Status != loka.SessionStatusTerminated {
		t.Errorf("Status = %q, want %q", got.Status, loka.SessionStatusTerminated)
	}

	// Verify a stop_session command was sent.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "stop_session" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected stop_session command to be sent to worker")
	}
}

func TestDestroyNonExistentSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	err := te.manager.Destroy(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error when destroying non-existent session")
	}
}

func TestSessionLifecycle(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	// Create
	s := te.createRunningSession(t, CreateOpts{Name: "lifecycle"})
	if s.Status != loka.SessionStatusRunning {
		t.Fatalf("after create: Status = %q, want %q", s.Status, loka.SessionStatusRunning)
	}

	// Get
	got, err := te.manager.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != s.ID {
		t.Fatalf("Get returned wrong session")
	}

	// List
	all, err := te.manager.List(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) < 1 {
		t.Fatal("List returned empty")
	}

	// Pause
	paused, err := te.manager.Pause(ctx, s.ID)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if paused.Status != loka.SessionStatusPaused {
		t.Fatalf("after pause: Status = %q", paused.Status)
	}

	// Resume
	resumed, err := te.manager.Resume(ctx, s.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != loka.SessionStatusRunning {
		t.Fatalf("after resume: Status = %q", resumed.Status)
	}

	// Destroy
	if err := te.manager.Destroy(ctx, s.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	destroyed, _ := te.manager.Get(ctx, s.ID)
	if destroyed.Status != loka.SessionStatusTerminated {
		t.Fatalf("after destroy: Status = %q", destroyed.Status)
	}
	te.drainWorkerCommands(t)
}

// ─── Mode transition tests ──────────────────────────────────────

func TestSetModeValid(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	// Start in explore mode.
	s := te.createRunningSession(t, CreateOpts{Name: "mode-test", Mode: loka.ModeExplore})

	// Explore -> Execute
	updated, err := te.manager.SetMode(ctx, s.ID, loka.ModeExecute)
	if err != nil {
		t.Fatalf("SetMode explore->execute: %v", err)
	}
	if updated.Mode != loka.ModeExecute {
		t.Errorf("Mode = %q, want %q", updated.Mode, loka.ModeExecute)
	}

	// Execute -> Ask
	updated, err = te.manager.SetMode(ctx, s.ID, loka.ModeAsk)
	if err != nil {
		t.Fatalf("SetMode execute->ask: %v", err)
	}
	if updated.Mode != loka.ModeAsk {
		t.Errorf("Mode = %q, want %q", updated.Mode, loka.ModeAsk)
	}

	// Ask -> Explore
	updated, err = te.manager.SetMode(ctx, s.ID, loka.ModeExplore)
	if err != nil {
		t.Fatalf("SetMode ask->explore: %v", err)
	}
	if updated.Mode != loka.ModeExplore {
		t.Errorf("Mode = %q, want %q", updated.Mode, loka.ModeExplore)
	}

	te.drainWorkerCommands(t)
}

func TestSetModeSameMode(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "same-mode", Mode: loka.ModeExplore})

	// Same mode transition should succeed (CanTransitionModeTo returns true for current==target).
	updated, err := te.manager.SetMode(ctx, s.ID, loka.ModeExplore)
	if err != nil {
		t.Fatalf("SetMode same mode: %v", err)
	}
	if updated.Mode != loka.ModeExplore {
		t.Errorf("Mode = %q, want %q", updated.Mode, loka.ModeExplore)
	}
	te.drainWorkerCommands(t)
}

func TestSetModeInvalidMode(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "invalid-mode", Mode: loka.ModeExplore})

	// An unrecognized mode should fail because it's not in ValidModeTransitions.
	_, err := te.manager.SetMode(ctx, s.ID, ExecMode("bogus"))
	if err == nil {
		t.Fatal("expected error for invalid mode transition")
	}
	te.drainWorkerCommands(t)
}

func TestSetModeSendsWorkerCommand(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "mode-cmd", Mode: loka.ModeExplore})

	_, err := te.manager.SetMode(ctx, s.ID, loka.ModeExecute)
	if err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "set_mode" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected set_mode command sent to worker")
	}
}

// ─── Exec tests ─────────────────────────────────────────────────

// Use an alias for readability.
type ExecMode = loka.ExecMode

func TestExecSingleCommand(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "exec-test", Mode: loka.ModeExecute})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo", Args: []string{"hello"}}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if exec.ID == "" {
		t.Fatal("expected non-empty execution ID")
	}
	if exec.SessionID != s.ID {
		t.Errorf("SessionID = %q, want %q", exec.SessionID, s.ID)
	}
	if exec.Status != loka.ExecStatusRunning {
		t.Errorf("Status = %q, want %q", exec.Status, loka.ExecStatusRunning)
	}
	if exec.Parallel {
		t.Error("Parallel should be false")
	}
	if len(exec.Commands) != 1 {
		t.Errorf("len(Commands) = %d, want 1", len(exec.Commands))
	}

	// Verify exec command dispatched.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "exec" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected exec command sent to worker")
	}
}

func TestExecParallelCommands(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "parallel-exec", Mode: loka.ModeExecute})

	commands := []loka.Command{
		{ID: "cmd-1", Command: "echo", Args: []string{"one"}},
		{ID: "cmd-2", Command: "echo", Args: []string{"two"}},
		{ID: "cmd-3", Command: "echo", Args: []string{"three"}},
	}
	exec, err := te.manager.Exec(ctx, s.ID, commands, true)
	if err != nil {
		t.Fatalf("Exec parallel: %v", err)
	}

	if !exec.Parallel {
		t.Error("Parallel should be true")
	}
	if len(exec.Commands) != 3 {
		t.Errorf("len(Commands) = %d, want 3", len(exec.Commands))
	}
	te.drainWorkerCommands(t)
}

func TestExecOnPausedSessionFails(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "paused-exec", Mode: loka.ModeExecute})

	_, err := te.manager.Pause(ctx, s.ID)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	_, err = te.manager.Exec(ctx, s.ID, commands, false)
	if err == nil {
		t.Fatal("expected error when executing on paused session")
	}
	te.drainWorkerCommands(t)
}

func TestExecOnTerminatedSessionFails(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "term-exec", Mode: loka.ModeExecute})

	if err := te.manager.Destroy(ctx, s.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	te.drainWorkerCommands(t)

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	_, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err == nil {
		t.Fatal("expected error when executing on terminated session")
	}
}

func TestExecPolicyBlockedCommand(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	policy := loka.DefaultExecPolicy()
	policy.BlockedCommands = []string{"rm"}

	s := te.createRunningSession(t, CreateOpts{
		Name:       "blocked-exec",
		Mode:       loka.ModeExecute,
		ExecPolicy: &policy,
	})

	commands := []loka.Command{{ID: "cmd-1", Command: "rm", Args: []string{"-rf", "/"}}}
	_, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err == nil {
		t.Fatal("expected policy violation error")
	}
	te.drainWorkerCommands(t)
}

func TestExecExploreModeAllowsAllCommands(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "explore-allow", Mode: loka.ModeExplore})

	// All commands are allowed in explore mode — filesystem is read-only (enforced by supervisor).
	commands := []loka.Command{{ID: "cmd-1", Command: "mkdir", Args: []string{"test"}}}
	_, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("all commands should be allowed in explore mode (filesystem is read-only): %v", err)
	}
	te.drainWorkerCommands(t)
}

func TestExecTooManyParallelCommands(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	policy := loka.DefaultExecPolicy()
	policy.MaxParallel = 2

	s := te.createRunningSession(t, CreateOpts{
		Name:       "max-parallel",
		Mode:       loka.ModeExecute,
		ExecPolicy: &policy,
	})

	commands := []loka.Command{
		{ID: "cmd-1", Command: "echo", Args: []string{"1"}},
		{ID: "cmd-2", Command: "echo", Args: []string{"2"}},
		{ID: "cmd-3", Command: "echo", Args: []string{"3"}},
	}
	_, err := te.manager.Exec(ctx, s.ID, commands, true)
	if err == nil {
		t.Fatal("expected error for too many parallel commands")
	}
	te.drainWorkerCommands(t)
}

// ─── Approval flow tests (ask mode) ─────────────────────────────

func TestExecAskModePendingApproval(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "ask-test", Mode: loka.ModeAsk})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo", Args: []string{"hello"}}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec in ask mode: %v", err)
	}

	if exec.Status != loka.ExecStatusPendingApproval {
		t.Errorf("Status = %q, want %q", exec.Status, loka.ExecStatusPendingApproval)
	}
	te.drainWorkerCommands(t)
}

func TestApproveExecution(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "approve-test", Mode: loka.ModeAsk})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo", Args: []string{"hi"}}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	approved, err := te.manager.ApproveExecution(ctx, s.ID, exec.ID)
	if err != nil {
		t.Fatalf("ApproveExecution: %v", err)
	}

	if approved.Status != loka.ExecStatusRunning {
		t.Errorf("Status = %q, want %q", approved.Status, loka.ExecStatusRunning)
	}

	// Verify approve_gate command sent.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "approve_gate" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected approve_gate command sent to worker")
	}
}

func TestApproveExecutionWithWhitelist(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "whitelist-test", Mode: loka.ModeAsk})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo", Args: []string{"hi"}}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	_, err = te.manager.ApproveExecution(ctx, s.ID, exec.ID, true)
	if err != nil {
		t.Fatalf("ApproveExecution with whitelist: %v", err)
	}

	// Verify the command binary was added to the session's exec policy AllowedCommands.
	updated, err := te.manager.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	found := false
	for _, cmd := range updated.ExecPolicy.AllowedCommands {
		if cmd == "echo" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'echo' to be added to AllowedCommands after whitelist approval")
	}
	te.drainWorkerCommands(t)
}

func TestApproveNonPendingExecutionFails(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "approve-fail", Mode: loka.ModeExecute})

	// Execute mode: execution goes directly to running.
	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	_, err = te.manager.ApproveExecution(ctx, s.ID, exec.ID)
	if err == nil {
		t.Fatal("expected error when approving non-pending_approval execution")
	}
}

func TestRejectExecution(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "reject-test", Mode: loka.ModeAsk})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	rejected, err := te.manager.RejectExecution(ctx, s.ID, exec.ID, "not allowed")
	if err != nil {
		t.Fatalf("RejectExecution: %v", err)
	}

	if rejected.Status != loka.ExecStatusRejected {
		t.Errorf("Status = %q, want %q", rejected.Status, loka.ExecStatusRejected)
	}
	if len(rejected.Results) == 0 {
		t.Fatal("expected rejection results")
	}
	if rejected.Results[0].ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", rejected.Results[0].ExitCode)
	}

	// Verify deny_gate command sent.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "deny_gate" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected deny_gate command sent to worker")
	}
}

func TestRejectExecutionDefaultReason(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "reject-default", Mode: loka.ModeAsk})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	rejected, err := te.manager.RejectExecution(ctx, s.ID, exec.ID, "")
	if err != nil {
		t.Fatalf("RejectExecution: %v", err)
	}

	if len(rejected.Results) == 0 {
		t.Fatal("expected rejection results")
	}
	if rejected.Results[0].Stderr != "execution rejected: denied by operator" {
		t.Errorf("Stderr = %q, want default reason", rejected.Results[0].Stderr)
	}
	te.drainWorkerCommands(t)
}

func TestRejectNonPendingExecutionFails(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "reject-fail", Mode: loka.ModeExecute})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	_, err = te.manager.RejectExecution(ctx, s.ID, exec.ID, "should fail")
	if err == nil {
		t.Fatal("expected error when rejecting non-pending_approval execution")
	}
}

// ─── Execution completion and cancellation ──────────────────────

func TestCompleteExecution(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "complete-test", Mode: loka.ModeExecute})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	results := []loka.CommandResult{{
		CommandID: "cmd-1",
		ExitCode:  0,
		Stdout:    "hello\n",
	}}
	err = te.manager.CompleteExecution(ctx, exec.ID, loka.ExecStatusSuccess, results, "")
	if err != nil {
		t.Fatalf("CompleteExecution: %v", err)
	}

	got, err := te.manager.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if got.Status != loka.ExecStatusSuccess {
		t.Errorf("Status = %q, want %q", got.Status, loka.ExecStatusSuccess)
	}
	if len(got.Results) != 1 {
		t.Errorf("len(Results) = %d, want 1", len(got.Results))
	}
}

func TestCancelExecution(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cancel-test", Mode: loka.ModeExecute})

	commands := []loka.Command{{ID: "cmd-1", Command: "sleep", Args: []string{"60"}}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	canceled, err := te.manager.CancelExecution(ctx, s.ID, exec.ID)
	if err != nil {
		t.Fatalf("CancelExecution: %v", err)
	}
	if canceled.Status != loka.ExecStatusCanceled {
		t.Errorf("Status = %q, want %q", canceled.Status, loka.ExecStatusCanceled)
	}

	// Verify cancel command sent.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "cancel_exec" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected cancel_exec command sent to worker")
	}
}

func TestCancelAlreadyTerminalExecution(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cancel-terminal", Mode: loka.ModeExecute})

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	exec, err := te.manager.Exec(ctx, s.ID, commands, false)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	te.drainWorkerCommands(t)

	// Complete it first.
	err = te.manager.CompleteExecution(ctx, exec.ID, loka.ExecStatusSuccess, nil, "")
	if err != nil {
		t.Fatalf("CompleteExecution: %v", err)
	}

	// Cancel should return the execution as-is without error.
	canceled, err := te.manager.CancelExecution(ctx, s.ID, exec.ID)
	if err != nil {
		t.Fatalf("CancelExecution on terminal: %v", err)
	}
	if canceled.Status != loka.ExecStatusSuccess {
		t.Errorf("Status = %q, want %q (unchanged)", canceled.Status, loka.ExecStatusSuccess)
	}
}

func TestListExecutions(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "list-exec", Mode: loka.ModeExecute})

	for i := 0; i < 3; i++ {
		commands := []loka.Command{{ID: "cmd", Command: "echo"}}
		_, err := te.manager.Exec(ctx, s.ID, commands, false)
		if err != nil {
			t.Fatalf("Exec %d: %v", i, err)
		}
	}
	te.drainWorkerCommands(t)

	execs, err := te.manager.ListExecutions(ctx, s.ID, store.ExecutionFilter{})
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 3 {
		t.Errorf("len(execs) = %d, want 3", len(execs))
	}
}

// ─── Checkpoint tests ───────────────────────────────────────────

func TestCreateCheckpoint(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cp-test", Mode: loka.ModeExecute})

	cp, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-1", loka.CheckpointLight, "")
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	if cp.ID != "cp-1" {
		t.Errorf("ID = %q, want %q", cp.ID, "cp-1")
	}
	if cp.SessionID != s.ID {
		t.Errorf("SessionID = %q, want %q", cp.SessionID, s.ID)
	}
	if cp.Type != loka.CheckpointLight {
		t.Errorf("Type = %q, want %q", cp.Type, loka.CheckpointLight)
	}
	if cp.Status != loka.CheckpointStatusCreating {
		t.Errorf("Status = %q, want %q", cp.Status, loka.CheckpointStatusCreating)
	}

	// Verify create_checkpoint command sent.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "create_checkpoint" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected create_checkpoint command sent to worker")
	}
}

func TestCompleteCheckpoint(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cp-complete", Mode: loka.ModeExecute})

	_, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-2", loka.CheckpointFull, "")
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	te.drainWorkerCommands(t)

	// Mark as ready.
	err = te.manager.CompleteCheckpoint(ctx, "cp-2", true, "overlays/cp-2.tar.zst", "")
	if err != nil {
		t.Fatalf("CompleteCheckpoint: %v", err)
	}

	// Retrieve and verify.
	cp, err := te.store.Checkpoints().Get(ctx, "cp-2")
	if err != nil {
		t.Fatalf("Get checkpoint: %v", err)
	}
	if cp.Status != loka.CheckpointStatusReady {
		t.Errorf("Status = %q, want %q", cp.Status, loka.CheckpointStatusReady)
	}
	if cp.OverlayPath != "overlays/cp-2.tar.zst" {
		t.Errorf("OverlayPath = %q, want %q", cp.OverlayPath, "overlays/cp-2.tar.zst")
	}
}

func TestCompleteCheckpointFailed(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cp-fail", Mode: loka.ModeExecute})

	_, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-3", loka.CheckpointLight, "")
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	te.drainWorkerCommands(t)

	err = te.manager.CompleteCheckpoint(ctx, "cp-3", false, "", "disk full")
	if err != nil {
		t.Fatalf("CompleteCheckpoint: %v", err)
	}

	cp, err := te.store.Checkpoints().Get(ctx, "cp-3")
	if err != nil {
		t.Fatalf("Get checkpoint: %v", err)
	}
	if cp.Status != loka.CheckpointStatusFailed {
		t.Errorf("Status = %q, want %q", cp.Status, loka.CheckpointStatusFailed)
	}
}

func TestRestoreCheckpoint(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cp-restore", Mode: loka.ModeExecute})

	_, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-4", loka.CheckpointLight, "")
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	te.drainWorkerCommands(t)

	// Complete the checkpoint so it's in ready state.
	err = te.manager.CompleteCheckpoint(ctx, "cp-4", true, "overlays/cp-4.tar.zst", "")
	if err != nil {
		t.Fatalf("CompleteCheckpoint: %v", err)
	}

	// Restore.
	err = te.manager.RestoreCheckpoint(ctx, s.ID, "cp-4")
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	// Verify restore_checkpoint command sent.
	cmds := te.drainWorkerCommands(t)
	found := false
	for _, cmd := range cmds {
		if cmd.Type == "restore_checkpoint" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected restore_checkpoint command sent to worker")
	}
}

func TestRestoreCheckpointNotReady(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cp-not-ready", Mode: loka.ModeExecute})

	// Create but do not complete checkpoint; it remains in "creating" status.
	_, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-5", loka.CheckpointLight, "")
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	te.drainWorkerCommands(t)

	err = te.manager.RestoreCheckpoint(ctx, s.ID, "cp-5")
	if err == nil {
		t.Fatal("expected error when restoring checkpoint that is not ready")
	}
}

func TestCreateCheckpointWithParent(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "cp-parent", Mode: loka.ModeExecute})

	// Create root checkpoint.
	_, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-root", loka.CheckpointLight, "")
	if err != nil {
		t.Fatalf("CreateCheckpoint root: %v", err)
	}
	te.drainWorkerCommands(t)

	// Create child checkpoint.
	cp, err := te.manager.CreateCheckpoint(ctx, s.ID, "cp-child", loka.CheckpointLight, "cp-root")
	if err != nil {
		t.Fatalf("CreateCheckpoint child: %v", err)
	}

	if cp.ParentID != "cp-root" {
		t.Errorf("ParentID = %q, want %q", cp.ParentID, "cp-root")
	}
	te.drainWorkerCommands(t)
}

// ─── Error and edge case tests ──────────────────────────────────

func TestGetExecutionNotFound(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	_, err := te.manager.GetExecution(ctx, "nonexistent-exec")
	if err == nil {
		t.Fatal("expected error for non-existent execution")
	}
}

func TestExecOnNonExistentSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	commands := []loka.Command{{ID: "cmd-1", Command: "echo"}}
	_, err := te.manager.Exec(ctx, "nonexistent-session", commands, false)
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
}

func TestPauseNonExistentSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	_, err := te.manager.Pause(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error when pausing non-existent session")
	}
}

func TestResumeNonExistentSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	_, err := te.manager.Resume(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error when resuming non-existent session")
	}
}

func TestSetModeNonExistentSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	_, err := te.manager.SetMode(ctx, "nonexistent-id", loka.ModeExecute)
	if err == nil {
		t.Fatal("expected error when setting mode on non-existent session")
	}
}

func TestCreateCheckpointNonExistentSession(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	_, err := te.manager.CreateCheckpoint(ctx, "nonexistent-id", "cp-x", loka.CheckpointLight, "")
	if err == nil {
		t.Fatal("expected error when creating checkpoint for non-existent session")
	}
}

func TestRestoreCheckpointNonExistentCheckpoint(t *testing.T) {
	te := setupTestManager(t)
	ctx := context.Background()

	s := te.createRunningSession(t, CreateOpts{Name: "restore-nocp", Mode: loka.ModeExecute})

	err := te.manager.RestoreCheckpoint(ctx, s.ID, "nonexistent-cp")
	if err == nil {
		t.Fatal("expected error when restoring non-existent checkpoint")
	}
	te.drainWorkerCommands(t)
}
