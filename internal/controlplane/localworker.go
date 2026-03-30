package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/controlplane/session"
	cpworker "github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/internal/worker"
	"github.com/vyprai/loka/internal/worker/vm"
	"github.com/vyprai/loka/pkg/lokavm"
)

// LocalWorker runs an embedded worker inside the control plane process.
// All execution goes through lokavm microVMs.
type LocalWorker struct {
	agent          *worker.Agent
	workerConn     *cpworker.WorkerConn
	registry       *cpworker.Registry
	sessionManager *session.Manager
	store          store.Store // For updating service records (e.g., ForwardPort).
	logger         *slog.Logger
	wg             sync.WaitGroup
}

// NewLocalWorker creates and registers an embedded worker with lokavm hypervisor.
func NewLocalWorker(registry *cpworker.Registry, sm *session.Manager, objStore objstore.ObjectStore, dataDir string, hypervisor lokavm.Hypervisor, logger *slog.Logger) (*LocalWorker, error) {
	agent, err := worker.NewAgent("local", map[string]string{"embedded": "true"}, dataDir, objStore, hypervisor, logger)
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

// Agent returns the embedded worker agent, allowing callers to access
// agent-level methods such as ServiceLogs.
func (lw *LocalWorker) Agent() *worker.Agent {
	return lw.agent
}

// SetStore sets the store used to update service records (e.g., ForwardPort).
func (lw *LocalWorker) SetStore(s store.Store) {
	lw.store = s
}

// Start begins processing commands and sending heartbeats.
func (lw *LocalWorker) Start(ctx context.Context) {
	lw.logger.Info("local worker started", "id", lw.agent.ID())

	// Command processing loop.
	lw.wg.Add(1)
	go func() {
		defer lw.wg.Done()
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

	// Heartbeat loop — keeps the worker alive in the health monitor.
	lw.wg.Add(1)
	go func() {
		defer lw.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hb := lw.agent.Heartbeat()
				if err := lw.registry.UpdateHeartbeat(ctx, lw.agent.ID(), hb); err != nil {
					lw.logger.Warn("heartbeat failed", "error", err)
				}
			}
		}
	}()
}

// Stop waits for all in-flight goroutines to finish with a timeout.
func (lw *LocalWorker) Stop() {
	done := make(chan struct{})
	go func() {
		lw.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		lw.logger.Warn("local worker stop timed out waiting for goroutines")
	}
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
			Mounts:              data.Mounts,
		}); err != nil {
			lw.logger.Error("failed to launch session", "session", data.SessionID, "error", err)
			// Mark session as error so API calls don't try to reach the worker.
			if lw.store != nil {
				if s, e := lw.store.Sessions().Get(ctx, data.SessionID); e == nil {
					s.Status = loka.SessionStatusError
					s.StatusMessage = err.Error()
					lw.store.Sessions().Update(ctx, s)
				}
			}
		}

	case "stop_session":
		data := cmd.Data.(map[string]string)
		if err := lw.agent.StopSession(data["session_id"]); err != nil {
			lw.logger.Error("failed to stop session", "session", data["session_id"], "error", err)
		}

	case "exec":
		data := cmd.Data.(cpworker.ExecCommandData)
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
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
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
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
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
			result := lw.agent.CreateCheckpoint(ctx, data.SessionID, data.CheckpointID, data.Type)
			if err := lw.sessionManager.CompleteCheckpoint(ctx, data.CheckpointID,
				result.Success, result.OverlayKey, result.Error); err != nil {
				lw.logger.Error("failed to report checkpoint complete", "checkpoint", data.CheckpointID, "error", err)
			}
		}()

	case "restore_checkpoint":
		data := cmd.Data.(cpworker.RestoreCheckpointData)
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
			if err := lw.agent.RestoreCheckpoint(ctx, data.SessionID, data.CheckpointID, data.OverlayKey); err != nil {
				lw.logger.Error("failed to restore checkpoint", "checkpoint", data.CheckpointID, "error", err)
			} else {
				lw.logger.Info("checkpoint restored on worker", "checkpoint", data.CheckpointID, "session", data.SessionID)
			}
		}()

	case "launch_service":
		data := cmd.Data.(cpworker.LaunchServiceData)
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
			if err := lw.agent.LaunchService(ctx, data.ServiceID, worker.ServiceLaunchOpts{
				ImageRef:             data.ImageRef,
				VCPUs:                data.VCPUs,
				MemoryMB:             data.MemoryMB,
				RootfsPath:           data.RootfsPath,
				LayerPackPath:        data.LayerPackPath,
				Command:              data.Command,
				Args:                 data.Args,
				Env:                  data.Env,
				Workdir:              data.Workdir,
				Port:                 data.Port,
				BundleKey:            data.BundleKey,
				RestartPolicy:        data.RestartPolicy,
				Mounts:               data.Mounts,
				SnapshotMemPath:      data.SnapshotMemPath,
				SnapshotVMStatePath:  data.SnapshotVMStatePath,
				IsAppSnapshotRestore: data.IsAppSnapshotRestore,
				HealthPath:           data.HealthPath,
			}); err != nil {
				lw.logger.Error("failed to launch service", "service", data.ServiceID, "error", err)
				if lw.store != nil {
					if svc, e := lw.store.Services().Get(ctx, data.ServiceID); e == nil {
						svc.Status = loka.ServiceStatusError
						svc.StatusMessage = err.Error()
						lw.store.Services().Update(ctx, svc)
					}
				}
				return
			}
			// Persist the guest IP and forwarded port so the domain proxy can route to the VM.
			if lw.store != nil {
				svc, err := lw.store.Services().Get(ctx, data.ServiceID)
				if err != nil {
					lw.logger.Warn("failed to get service for routing update",
						"service", data.ServiceID, "error", err)
				} else {
					updated := false
					if guestIP := lw.agent.GetGuestIP(data.ServiceID); guestIP != "" {
						svc.GuestIP = guestIP
						updated = true
					}
					if fwdPort := lw.agent.GetForwardedPort(data.ServiceID); fwdPort > 0 {
						svc.ForwardPort = fwdPort
						updated = true
					}
					if updated {
						if err := lw.store.Services().Update(ctx, svc); err != nil {
							lw.logger.Warn("failed to persist service routing info",
								"service", data.ServiceID, "error", err)
						} else {
							lw.logger.Info("service routing info persisted",
								"service", data.ServiceID,
								"guest_ip", svc.GuestIP,
								"forward_port", svc.ForwardPort)
						}
					}
				}
			}
		}()

	case "snapshot_service":
		data := cmd.Data.(cpworker.SnapshotServiceData)
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
			lw.handleSnapshotService(ctx, data.ServiceID)
		}()

	case "stop_service":
		data := cmd.Data.(cpworker.StopServiceData)
		if err := lw.agent.StopService(data.ServiceID); err != nil {
			lw.logger.Error("failed to stop service", "service", data.ServiceID, "error", err)
		}

	case "service_exec":
		data := cmd.Data.(cpworker.ServiceExecData)
		result := lw.agent.ExecCommands(ctx, data.ServiceID, fmt.Sprintf("svc-exec-%d", time.Now().UnixNano()), data.Commands, false)
		if result.Error != "" {
			lw.logger.Error("service exec failed", "service", data.ServiceID, "error", result.Error)
		} else {
			lw.logger.Info("service exec completed", "service", data.ServiceID, "status", result.Status)
		}

	case "service_status":
		data := cmd.Data.(map[string]string)
		serviceID := data["service_id"]
		status, err := lw.agent.ServiceStatus(serviceID)
		if err != nil {
			lw.logger.Debug("service status check failed", "service", serviceID, "error", err)
		} else {
			lw.logger.Debug("service status", "service", serviceID, "running", status.Running, "pid", status.PID)
		}

	case "service_logs":
		data := cmd.Data.(map[string]interface{})
		serviceID := data["service_id"].(string)
		lines := 100
		if l, ok := data["lines"].(float64); ok {
			lines = int(l)
		}
		result, err := lw.agent.ServiceLogs(serviceID, lines)
		if err != nil {
			lw.logger.Debug("service logs request failed", "service", serviceID, "error", err)
		} else {
			lw.logger.Debug("service logs retrieved", "service", serviceID, "stdout_lines", len(result.Stdout), "stderr_lines", len(result.Stderr))
		}

	case "shell_start":
		data := cmd.Data.(cpworker.ShellStartData)
		lw.wg.Add(1)
		go func() {
			defer lw.wg.Done()
			// Bridge cpworker.ShellFrame channels to vm.ShellFrame channels.
			vmInput := make(chan vm.ShellFrame, 64)
			vmOutput := make(chan vm.ShellFrame, 64)

			// Translate CP input → VM input.
			go func() {
				defer close(vmInput)
				for f := range data.Relay.Input {
					vmInput <- vm.ShellFrame{Type: f.Type, Data: f.Data}
				}
			}()

			// Translate VM output → CP output.
			go func() {
				for f := range vmOutput {
					data.Relay.Output <- cpworker.ShellFrame{Type: f.Type, Data: f.Data}
				}
				close(data.Relay.Output)
			}()

			// Start shell — blocks until shell exits.
			// Pass the relay's ErrCh directly so the CP gets notified immediately.
			lw.agent.StartShell(data.SessionID, data.Command, data.Rows, data.Cols,
				data.Workdir, data.Env, vmInput, vmOutput, data.Relay.ErrCh)
		}()

	default:
		lw.logger.Warn("unknown command type", "type", cmd.Type)
	}
}

// handleSnapshotService takes an app-level snapshot of a running service,
// compresses and uploads it to the object store, updates the service record,
// and then stops the VM.
func (lw *LocalWorker) handleSnapshotService(ctx context.Context, serviceID string) {
	// Check if the agent already has an app snapshot from the initial deploy health check.
	memPath, statePath := lw.agent.GetAppSnapshotPaths(serviceID)
	if memPath == "" || statePath == "" {
		// No existing snapshot — create one now.
		if hv := lw.agent.Hypervisor(); hv != nil {
			snap, err := hv.CreateSnapshot(serviceID)
			if err != nil {
				lw.logger.Error("snapshot_service: failed", "service", serviceID, "error", err)
				lw.agent.StopService(serviceID)
				lw.agent.StopSession(serviceID)
				return
			}
			memPath = snap.MemPath
			statePath = snap.StatePath
		}
	}

	// Compress and upload to objstore.
	if lw.store != nil {
		svc, err := lw.store.Services().Get(ctx, serviceID)
		if err != nil {
			lw.logger.Warn("snapshot_service: failed to get service record",
				"service", serviceID, "error", err)
		} else {
			svc.AppSnapshotMem = fmt.Sprintf("services/%s/app_snapshot_mem", serviceID)
			svc.AppSnapshotState = fmt.Sprintf("services/%s/app_snapshot_vmstate", serviceID)
			// Upload raw snapshot files to objstore if available.
			if lw.agent != nil && lw.agent.ObjStore() != nil {
				if err := uploadFile(ctx, lw.agent.ObjStore(), "services", fmt.Sprintf("%s/app_snapshot_mem", serviceID), memPath); err != nil {
					lw.logger.Warn("snapshot_service: failed to upload mem snapshot",
						"service", serviceID, "error", err)
					svc.AppSnapshotMem = ""
				}
				if err := uploadFile(ctx, lw.agent.ObjStore(), "services", fmt.Sprintf("%s/app_snapshot_vmstate", serviceID), statePath); err != nil {
					lw.logger.Warn("snapshot_service: failed to upload vmstate snapshot",
						"service", serviceID, "error", err)
					svc.AppSnapshotState = ""
				}
			}
			svc.UpdatedAt = time.Now()
			if err := lw.store.Services().Update(ctx, svc); err != nil {
				lw.logger.Warn("snapshot_service: failed to update service record",
					"service", serviceID, "error", err)
			} else {
				lw.logger.Info("app snapshot persisted to service record",
					"service", serviceID,
					"mem_key", svc.AppSnapshotMem,
					"state_key", svc.AppSnapshotState)
			}
		}
	}

	// Stop the VM.
	lw.agent.StopService(serviceID)
	lw.agent.StopSession(serviceID)
	lw.logger.Info("service stopped after snapshot", "service", serviceID)
}

// uploadFile uploads a local file to the object store.
func uploadFile(ctx context.Context, store objstore.ObjectStore, bucket, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", localPath, err)
	}
	return store.Put(ctx, bucket, key, f, info.Size())
}
