package controlplane

import (
	"context"
	"log/slog"

	"github.com/vyprai/loka/internal/controlplane/session"
	cpworker "github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker"
	"github.com/vyprai/loka/internal/worker/vm"
)

// LocalWorker runs an embedded worker inside the control plane process.
// Even in local mode, all execution goes through Firecracker microVMs.
type LocalWorker struct {
	agent          *worker.Agent
	workerConn     *cpworker.WorkerConn
	registry       *cpworker.Registry
	sessionManager *session.Manager
	logger         *slog.Logger
}

// NewLocalWorker creates and registers an embedded worker with Firecracker support.
func NewLocalWorker(registry *cpworker.Registry, sm *session.Manager, objStore objstore.ObjectStore, dataDir string, fcConfig vm.FirecrackerConfig, logger *slog.Logger) (*LocalWorker, error) {
	agent, err := worker.NewAgent("local", map[string]string{"embedded": "true"}, dataDir, objStore, fcConfig, logger)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	w, err := registry.Register(ctx,
		agent.Hostname(), "127.0.0.1", "local", "local", "",
		"embedded", agent.Capacity(), agent.Labels(), false,
	)
	if err != nil {
		return nil, err
	}
	agent.SetID(w.ID)

	conn, _ := registry.Get(w.ID)

	lw := &LocalWorker{
		agent:          agent,
		workerConn:     conn,
		registry:       registry,
		sessionManager: sm,
		logger:         logger,
	}

	return lw, nil
}

// Start begins processing commands from the registry channel.
func (lw *LocalWorker) Start(ctx context.Context) {
	lw.logger.Info("local worker started", "id", lw.agent.ID())

	go func() {
		for {
			select {
			case <-ctx.Done():
				lw.logger.Info("local worker stopped")
				return
			case cmd, ok := <-lw.workerConn.CmdChan:
				if !ok {
					return
				}
				lw.handleCommand(ctx, cmd)
			}
		}
	}()
}

func (lw *LocalWorker) handleCommand(ctx context.Context, cmd cpworker.WorkerCommand) {
	switch cmd.Type {
	case "launch_session":
		data := cmd.Data.(cpworker.LaunchSessionData)
		if err := lw.agent.LaunchSession(ctx, data.SessionID, worker.LaunchOpts{
			Mode:                data.Mode,
			Policy:              data.ExecPolicy,
			VCPU:                data.VCPUs,
			MemoryMB:            data.MemoryMB,
			RootfsPath:          data.RootfsPath,
			SnapshotMemPath:     data.SnapshotMemPath,
			SnapshotVMStatePath: data.SnapshotVMStatePath,
		}); err != nil {
			lw.logger.Error("failed to launch session", "session", data.SessionID, "error", err)
		}

	case "stop_session":
		data := cmd.Data.(map[string]string)
		if err := lw.agent.StopSession(data["session_id"]); err != nil {
			lw.logger.Error("failed to stop session", "session", data["session_id"], "error", err)
		}

	case "exec":
		data := cmd.Data.(cpworker.ExecCommandData)
		go func() {
			result := lw.agent.ExecCommands(ctx, data.SessionID, data.ExecID, data.Commands, data.Parallel)
			// Report results back to session manager.
			if err := lw.sessionManager.CompleteExecution(ctx, data.ExecID,
				result.Status, result.Results, result.Error); err != nil {
				lw.logger.Error("failed to report exec complete", "exec", data.ExecID, "error", err)
			}
		}()

	case "set_mode":
		data := cmd.Data.(map[string]interface{})
		sessionID := data["session_id"].(string)
		mode := loka.ExecMode(data["mode"].(string))
		if err := lw.agent.SetMode(sessionID, mode); err != nil {
			lw.logger.Error("failed to set mode", "session", sessionID, "error", err)
		}

	case "cancel_exec":
		data := cmd.Data.(map[string]string)
		if err := lw.agent.CancelAllExec(data["session_id"]); err != nil {
			lw.logger.Error("failed to cancel exec", "session", data["session_id"], "error", err)
		}

	case "approve_exec":
		data := cmd.Data.(cpworker.ApproveExecData)
		go func() {
			// Mark command IDs as approved on the proxy, then re-execute.
			if err := lw.agent.ApproveOnProxy(data.SessionID, data.CommandIDs); err != nil {
				lw.logger.Error("failed to approve on proxy", "exec", data.ExecID, "error", err)
				return
			}
			result := lw.agent.ExecCommands(ctx, data.SessionID, data.ExecID, data.Commands, data.Parallel)
			if err := lw.sessionManager.CompleteExecution(ctx, data.ExecID,
				result.Status, result.Results, result.Error); err != nil {
				lw.logger.Error("failed to report exec after approve", "exec", data.ExecID, "error", err)
			}
		}()

	case "add_to_whitelist":
		data := cmd.Data.(cpworker.AddToWhitelistData)
		if err := lw.agent.AddToWhitelist(data.SessionID, data.Command); err != nil {
			lw.logger.Error("failed to add to whitelist", "session", data.SessionID, "error", err)
		}

	case "approve_gate":
		data := cmd.Data.(cpworker.ApproveOnGateData)
		if err := lw.agent.ApproveOnGate(data.SessionID, data.CommandID, data.AddToWhitelist); err != nil {
			lw.logger.Error("failed to approve on gate", "session", data.SessionID, "command", data.CommandID, "error", err)
		}

	case "deny_gate":
		data := cmd.Data.(cpworker.DenyOnGateData)
		if err := lw.agent.DenyOnGate(data.SessionID, data.CommandID, data.Reason); err != nil {
			lw.logger.Error("failed to deny on gate", "session", data.SessionID, "command", data.CommandID, "error", err)
		}

	case "create_checkpoint":
		data := cmd.Data.(cpworker.CreateCheckpointData)
		go func() {
			result := lw.agent.CreateCheckpoint(ctx, data.SessionID, data.CheckpointID, data.Type)
			if err := lw.sessionManager.CompleteCheckpoint(ctx, data.CheckpointID,
				result.Success, result.OverlayKey, result.Error); err != nil {
				lw.logger.Error("failed to report checkpoint complete", "checkpoint", data.CheckpointID, "error", err)
			}
		}()

	case "restore_checkpoint":
		data := cmd.Data.(cpworker.RestoreCheckpointData)
		go func() {
			if err := lw.agent.RestoreCheckpoint(ctx, data.SessionID, data.CheckpointID, data.OverlayKey); err != nil {
				lw.logger.Error("failed to restore checkpoint", "checkpoint", data.CheckpointID, "error", err)
			} else {
				lw.logger.Info("checkpoint restored on worker", "checkpoint", data.CheckpointID, "session", data.SessionID)
			}
		}()

	default:
		lw.logger.Warn("unknown command type", "type", cmd.Type)
	}
}
