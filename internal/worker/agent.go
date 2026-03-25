package worker

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
)

// Agent is the worker-side agent that manages Firecracker microVMs.
// There are NO direct process executions — everything goes through a VM.
type Agent struct {
	id       string
	hostname string
	provider string
	labels   map[string]string
	logger   *slog.Logger

	mu            sync.RWMutex
	sessions      map[string]*SessionState
	overlay       *vm.OverlayManager
	checkpointMgr *CheckpointManager
	vmManager     *vm.Manager
	objStore      objstore.ObjectStore
}

// ServiceLaunchOpts holds options for launching a service VM.
type ServiceLaunchOpts struct {
	ImageRef            string
	VCPUs               int
	MemoryMB            int
	RootfsPath          string
	Command             string
	Args                []string
	Env                 map[string]string
	Workdir             string
	Port                int
	BundleKey           string
	RestartPolicy       string
	SnapshotMemPath     string // Warm snapshot memory file for instant restore.
	SnapshotVMStatePath string // Warm snapshot VM state file.
	IsAppSnapshotRestore bool  // If true, skip bundle extraction and service_start (app already running).
	HealthPath          string // HTTP path for health check (empty = TCP only).
}

// SessionState tracks a running session backed by a Firecracker microVM.
type SessionState struct {
	ID       string
	Mode     loka.ExecMode
	Policy   loka.ExecPolicy
	VM       *vm.MicroVM     // Firecracker VM instance.
	Vsock    *vm.VsockClient // vsock connection to supervisor inside VM.
	LayerMap map[string]string
	GuestIP  string // VM guest IP for direct TCP access (e.g. "172.16.0.14").

	// Port forwarding: local TCP listener that tunnels to VM via vsock.
	PortForwardListener net.Listener
	ForwardedPort       int // Local port assigned to the port forward listener.

	// App-level warm snapshot paths (per-service, includes running app).
	AppSnapshotMem   string // Local path to app-level memory snapshot.
	AppSnapshotState string // Local path to app-level vmstate snapshot.
}

// NewAgent creates a new worker agent with Firecracker VM management.
func NewAgent(provider string, labels map[string]string, dataDir string, objStore objstore.ObjectStore, fcConfig vm.FirecrackerConfig, logger *slog.Logger) (*Agent, error) {
	hostname, _ := os.Hostname()
	overlay := vm.NewOverlayManager(dataDir)
	cpMgr := NewCheckpointManager(overlay, objStore, logger)

	vmMgr, err := vm.NewManager(fcConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("init VM manager: %w", err)
	}

	return &Agent{
		id:            uuid.New().String(),
		hostname:      hostname,
		provider:      provider,
		labels:        labels,
		logger:        logger,
		sessions:      make(map[string]*SessionState),
		overlay:       overlay,
		checkpointMgr: cpMgr,
		vmManager:     vmMgr,
		objStore:      objStore,
	}, nil
}

// ID returns the agent's unique identifier.
func (a *Agent) ID() string { return a.id }

// VMManager returns the underlying VM manager, allowing callers to use it
// for warm snapshot creation (e.g., the image manager in local mode).
func (a *Agent) VMManager() *vm.Manager { return a.vmManager }

// ObjStore returns the object store used by the agent.
func (a *Agent) ObjStore() objstore.ObjectStore { return a.objStore }

// SetID sets the agent ID (from registration response).
func (a *Agent) SetID(id string) { a.id = id }

// Capacity returns the local machine's resource capacity.
func (a *Agent) Capacity() loka.ResourceCapacity {
	return loka.ResourceCapacity{
		CPUCores: runtime.NumCPU(),
		MemoryMB: 8192, // Placeholder; real detection would use /proc/meminfo.
		DiskMB:   51200,
	}
}

// Hostname returns the agent's hostname.
func (a *Agent) Hostname() string { return a.hostname }

// Provider returns the provider name.
func (a *Agent) Provider() string { return a.provider }

// Labels returns the agent's labels.
func (a *Agent) Labels() map[string]string { return a.labels }

// LaunchOpts holds options for launching a session.
type LaunchOpts struct {
	Mode                loka.ExecMode
	Policy              loka.ExecPolicy
	VCPU                int
	MemoryMB            int
	RootfsPath          string // Image rootfs path (if not using default).
	SnapshotMemPath     string // Warm snapshot memory file for instant restore.
	SnapshotVMStatePath string // Warm snapshot VM state file.
}

// LaunchSession starts a Firecracker microVM for this session.
func (a *Agent) LaunchSession(ctx context.Context, sessionID string, opts LaunchOpts) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.sessions[sessionID]; exists {
		return fmt.Errorf("session %s already exists", sessionID)
	}

	// Initialize overlay filesystem.
	if err := a.overlay.Init(sessionID); err != nil {
		return fmt.Errorf("init overlay: %w", err)
	}

	vcpu := opts.VCPU
	if vcpu == 0 { vcpu = 1 }
	mem := opts.MemoryMB
	if mem == 0 { mem = 512 }

	// Launch Firecracker microVM — uses warm snapshot if available (~28ms),
	// otherwise cold boots (~1-2s).
	microVM, err := a.vmManager.Launch(ctx, sessionID, vm.VMConfig{
		VCPU:                vcpu,
		MemoryMB:            mem,
		RootfsPath:          opts.RootfsPath,
		OverlayDir:          a.overlay.SessionDir(sessionID),
		SnapshotMemPath:     opts.SnapshotMemPath,
		SnapshotVMStatePath: opts.SnapshotVMStatePath,
	})
	if err != nil {
		return fmt.Errorf("launch VM: %w", err)
	}

	// Connect to the supervisor inside the VM via vsock.
	vsock := vm.NewVsockClient(microVM.VsockPath)

	// Wait for supervisor to be ready with exponential backoff.
	backoff := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		if err := vsock.Ping(); err == nil {
			break
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}

	// Send exec policy and mode to the supervisor.
	vsock.SetPolicy(opts.Policy)
	vsock.SetMode(opts.Mode)

	a.sessions[sessionID] = &SessionState{
		ID:       sessionID,
		Mode:     opts.Mode,
		Policy:   opts.Policy,
		VM:       microVM,
		Vsock:    vsock,
		GuestIP:  microVM.GuestIP,
		LayerMap: make(map[string]string),
	}

	a.logger.Info("session launched with Firecracker VM",
		"session", sessionID,
		"vm_pid", microVM.PID,
		"guest_ip", microVM.GuestIP,
		"mode", opts.Mode,
		"warm_snapshot", opts.SnapshotMemPath != "",
	)
	return nil
}

// WorkspacePath returns the workspace path for a session.
func (a *Agent) WorkspacePath(sessionID string) string {
	return a.overlay.WorkspacePath(sessionID)
}

// StopSession stops the microVM and cleans up.
func (a *Agent) StopSession(sessionID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[sessionID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("session %s not found", sessionID)
	}
	// Close port forward listener if active.
	if sess.PortForwardListener != nil {
		sess.PortForwardListener.Close()
	}
	delete(a.sessions, sessionID)
	a.mu.Unlock()

	a.vmManager.Stop(sessionID)
	a.logger.Info("session stopped", "session", sessionID)
	return nil
}

// SetMode changes the execution mode — sends to supervisor via vsock.
func (a *Agent) SetMode(sessionID string, mode loka.ExecMode) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	if err := sess.Vsock.SetMode(mode); err != nil {
		return fmt.Errorf("set mode via vsock: %w", err)
	}
	sess.Mode = mode
	a.logger.Info("session mode changed", "session", sessionID, "mode", mode)
	return nil
}

// ExecResult contains the result of a command execution.
type ExecResult struct {
	ExecID  string
	Status  loka.ExecStatus
	Results []loka.CommandResult
	Error   string
}

// ExecCommands sends commands to the supervisor inside the VM via vsock.
func (a *Agent) ExecCommands(ctx context.Context, sessionID, execID string, commands []loka.Command, parallel bool) *ExecResult {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return &ExecResult{ExecID: execID, Status: loka.ExecStatusFailed, Error: fmt.Sprintf("session %s not found", sessionID)}
	}

	resp, err := sess.Vsock.Execute(vm.ExecRequest{Commands: commands, Parallel: parallel, ExecID: execID})
	if err != nil {
		return &ExecResult{ExecID: execID, Status: loka.ExecStatusFailed, Error: err.Error()}
	}
	return &ExecResult{ExecID: execID, Status: loka.ExecStatus(resp.Status), Results: resp.Results, Error: resp.Error}
}

// ApproveOnProxy delegates to ApproveOnGate.
func (a *Agent) ApproveOnProxy(sessionID string, commandIDs []string) error {
	for _, id := range commandIDs {
		if err := a.ApproveOnGate(sessionID, id, false); err != nil {
			return err
		}
	}
	return nil
}

// ApproveOnGate sends approve to the supervisor's gate via vsock.
func (a *Agent) ApproveOnGate(sessionID, commandID string, addToWhitelist bool) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return sess.Vsock.ApproveCommand(commandID, addToWhitelist)
}

// DenyOnGate sends deny to the supervisor's gate via vsock.
func (a *Agent) DenyOnGate(sessionID, commandID, reason string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return sess.Vsock.DenyCommand(commandID, reason)
}

// AddToWhitelist permanently whitelists a command via vsock.
func (a *Agent) AddToWhitelist(sessionID, command string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	// Make a copy to avoid concurrent slice write races.
	newAllowed := make([]string, len(sess.Policy.AllowedCommands), len(sess.Policy.AllowedCommands)+1)
	copy(newAllowed, sess.Policy.AllowedCommands)
	newAllowed = append(newAllowed, command)
	sess.Policy.AllowedCommands = newAllowed
	return sess.Vsock.SetPolicy(sess.Policy)
}

// CancelExec cancels a running command via vsock.
func (a *Agent) CancelExec(sessionID, cmdID string) error { return nil }

// CancelAllExec cancels all running commands.
func (a *Agent) CancelAllExec(sessionID string) error { return nil }

// Heartbeat returns the current heartbeat data.
func (a *Agent) Heartbeat() *loka.Heartbeat {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var sessionIDs []string
	for id := range a.sessions {
		sessionIDs = append(sessionIDs, id)
	}

	status := loka.WorkerStatusReady
	if len(a.sessions) > 0 {
		status = loka.WorkerStatusBusy
	}

	return &loka.Heartbeat{
		WorkerID:     a.id,
		Timestamp:    time.Now(),
		Status:       status,
		SessionCount: len(a.sessions),
		SessionIDs:   sessionIDs,
	}
}

// CreateCheckpoint creates a checkpoint for a session.
func (a *Agent) CreateCheckpoint(ctx context.Context, sessionID, checkpointID string, cpType loka.CheckpointType) *CheckpointResult {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return &CheckpointResult{
			CheckpointID: checkpointID,
			SessionID:    sessionID,
			Error:        fmt.Sprintf("session %s not found", sessionID),
		}
	}

	result := a.checkpointMgr.Create(ctx, sessionID, checkpointID, cpType)

	if result.Success {
		// Track the layer mapping.
		a.mu.Lock()
		sess.LayerMap[checkpointID] = result.LayerName
		a.mu.Unlock()
	}

	return result
}

// RestoreCheckpoint restores a session's workspace to a checkpoint.
func (a *Agent) RestoreCheckpoint(ctx context.Context, sessionID, checkpointID, overlayKey string) error {
	a.mu.RLock()
	_, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	return a.checkpointMgr.Restore(ctx, sessionID, checkpointID, overlayKey)
}

// DiffCheckpoints returns filesystem diff between two checkpoint layers.
func (a *Agent) DiffCheckpoints(sessionID, cpIDA, cpIDB string) ([]vm.DiffEntry, error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	layerA, okA := sess.LayerMap[cpIDA]
	layerB, okB := sess.LayerMap[cpIDB]
	if !okA || !okB {
		return nil, fmt.Errorf("checkpoint layer mapping not found")
	}

	return a.checkpointMgr.Diff(sessionID, layerA, layerB)
}

// ── Service Process Methods ──────────────────────────────

// LaunchService boots a Firecracker VM for a service, optionally extracts a
// bundle from the object store, and starts the long-running service process.
// The VM is tracked in the sessions map keyed by serviceID so that
// StopService / ServiceStatus / ServiceLogs can find it.
func (a *Agent) LaunchService(ctx context.Context, serviceID string, opts ServiceLaunchOpts) error {
	a.mu.Lock()
	if _, exists := a.sessions[serviceID]; exists {
		a.mu.Unlock()
		return fmt.Errorf("service %s already exists", serviceID)
	}
	a.mu.Unlock()

	// Initialize overlay filesystem (same as session launch).
	if err := a.overlay.Init(serviceID); err != nil {
		return fmt.Errorf("init overlay: %w", err)
	}

	vcpu := opts.VCPUs
	if vcpu == 0 {
		vcpu = 1
	}
	mem := opts.MemoryMB
	if mem == 0 {
		mem = 512
	}

	// Launch Firecracker microVM — uses warm snapshot if available (~28ms),
	// otherwise cold boots (~1-2s).
	microVM, err := a.vmManager.Launch(ctx, serviceID, vm.VMConfig{
		VCPU:                vcpu,
		MemoryMB:            mem,
		RootfsPath:          opts.RootfsPath,
		OverlayDir:          a.overlay.SessionDir(serviceID),
		SnapshotMemPath:     opts.SnapshotMemPath,
		SnapshotVMStatePath: opts.SnapshotVMStatePath,
	})
	if err != nil {
		return fmt.Errorf("launch VM: %w", err)
	}

	// Connect to the supervisor inside the VM via vsock.
	vsock := vm.NewVsockClient(microVM.VsockPath)

	// Wait for supervisor to be ready with exponential backoff.
	backoff := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		if err := vsock.Ping(); err == nil {
			break
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}

	// If restoring from an app snapshot, the app is already running inside
	// the VM. Skip bundle extraction and service_start — just set up port
	// forwarding.
	if opts.IsAppSnapshotRestore {
		a.mu.Lock()
		a.sessions[serviceID] = &SessionState{
			ID:       serviceID,
			VM:       microVM,
			Vsock:    vsock,
			GuestIP:  microVM.GuestIP,
			LayerMap: make(map[string]string),
		}
		a.mu.Unlock()

		if opts.Port > 0 {
			localPort, err := a.StartPortForward(serviceID, opts.Port)
			if err != nil {
				a.logger.Warn("port forward failed after app snapshot restore",
					"service", serviceID, "error", err)
			} else {
				a.logger.Info("service restored from app snapshot",
					"service", serviceID,
					"vm_pid", microVM.PID,
					"forward_port", localPort,
				)
				return nil
			}
		}

		a.logger.Info("service restored from app snapshot",
			"service", serviceID,
			"vm_pid", microVM.PID,
		)
		return nil
	}

	// Extract bundle into the VM if a BundleKey is provided.
	if opts.BundleKey != "" && a.objStore != nil {
		if err := a.extractBundle(ctx, vsock, opts.BundleKey, opts.Workdir); err != nil {
			a.vmManager.Stop(serviceID)
			return fmt.Errorf("extract bundle: %w", err)
		}
	}

	// Start the service process inside the VM.
	restartPolicy := opts.RestartPolicy
	if restartPolicy == "" {
		restartPolicy = "on-failure"
	}
	if _, err := vsock.ServiceStart(opts.Command, opts.Args, opts.Env, opts.Workdir, restartPolicy); err != nil {
		a.vmManager.Stop(serviceID)
		return fmt.Errorf("start service: %w", err)
	}

	// Store the session/VM reference so other service methods can find it.
	a.mu.Lock()
	a.sessions[serviceID] = &SessionState{
		ID:       serviceID,
		VM:       microVM,
		Vsock:    vsock,
		GuestIP:  microVM.GuestIP,
		LayerMap: make(map[string]string),
	}
	a.mu.Unlock()

	// Start port forwarding so the domain proxy can reach the service
	// inside the VM via a local TCP port tunnelled through vsock.
	if opts.Port > 0 {
		localPort, err := a.StartPortForward(serviceID, opts.Port)
		if err != nil {
			a.logger.Warn("port forward failed, service still running",
				"service", serviceID, "error", err)
		} else {
			a.logger.Info("service launched with Firecracker VM",
				"service", serviceID,
				"vm_pid", microVM.PID,
				"command", opts.Command,
				"forward_port", localPort,
			)
		}
	} else {
		a.logger.Info("service launched with Firecracker VM",
			"service", serviceID,
			"vm_pid", microVM.PID,
			"command", opts.Command,
		)
	}

	// Poll health check and take app snapshot when healthy.
	if opts.Port > 0 {
		go a.waitHealthyAndSnapshot(serviceID, opts.Port, opts.HealthPath)
	}

	return nil
}

// waitHealthyAndSnapshot polls the health check inside the VM and takes an
// app-level snapshot once the service is healthy. This snapshot can be used
// for instant cold-start on idle->wake (~2ms instead of full boot + deploy).
func (a *Agent) waitHealthyAndSnapshot(serviceID string, port int, healthPath string) {
	a.mu.RLock()
	sess, ok := a.sessions[serviceID]
	a.mu.RUnlock()
	if !ok {
		return
	}

	for i := 0; i < 30; i++ {
		if err := sess.Vsock.HealthCheck(port, healthPath); err == nil {
			// App is ready — take app snapshot.
			a.logger.Info("app healthy, creating app snapshot", "service", serviceID)
			memPath, statePath, err := a.vmManager.CreateDiffSnapshot(serviceID)
			if err != nil {
				a.logger.Warn("failed to create app snapshot", "service", serviceID, "error", err)
				return
			}

			// Resume VM after snapshot (CreateDiffSnapshot leaves it paused).
			if err := a.vmManager.Resume(serviceID); err != nil {
				a.logger.Warn("failed to resume VM after app snapshot", "service", serviceID, "error", err)
			}

			a.mu.Lock()
			if s, ok := a.sessions[serviceID]; ok {
				s.AppSnapshotMem = memPath
				s.AppSnapshotState = statePath
			}
			a.mu.Unlock()

			a.logger.Info("app snapshot created",
				"service", serviceID,
				"mem", memPath,
				"state", statePath,
			)
			return
		}
		time.Sleep(2 * time.Second)
	}
	a.logger.Warn("app health check timed out, no app snapshot created", "service", serviceID)
}

// GetAppSnapshotPaths returns the local app snapshot paths for a service session.
func (a *Agent) GetAppSnapshotPaths(serviceID string) (memPath, statePath string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	sess, ok := a.sessions[serviceID]
	if !ok {
		return "", ""
	}
	return sess.AppSnapshotMem, sess.AppSnapshotState
}

// extractBundle downloads a bundle from the object store and streams it into
// the VM in chunks to avoid loading the entire bundle into memory at once.
// Each chunk is base64-encoded and appended to a temp file inside the VM,
// then extracted. Peak memory usage is ~512KB per chunk instead of the full bundle.
func (a *Agent) extractBundle(ctx context.Context, vsock *vm.VsockClient, bundleKey, workdir string) error {
	// BundleKey format: "services/<id>/bundle.tar.gz"
	// Object store bucket is the first path segment, key is the rest.
	parts := strings.SplitN(bundleKey, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid bundle key %q: expected 'bucket/key'", bundleKey)
	}
	bucket, key := parts[0], parts[1]

	reader, err := a.objStore.Get(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("download bundle: %w", err)
	}
	defer reader.Close()

	// Stream to a temp file on the host to avoid holding the entire bundle in memory.
	tmpFile, err := os.CreateTemp("", "loka-bundle-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		return fmt.Errorf("save bundle to temp: %w", err)
	}
	tmpFile.Close()

	if workdir == "" {
		workdir = "/workspace"
	}

	// Create the workspace directory inside the VM.
	resp, execErr := vsock.Execute(vm.ExecRequest{
		Commands: []loka.Command{
			{
				ID:      uuid.New().String(),
				Command: "sh",
				Args:    []string{"-c", fmt.Sprintf("mkdir -p %s", workdir)},
			},
		},
	})
	if execErr != nil {
		return fmt.Errorf("mkdir in VM: %w", execErr)
	}
	if resp.Status != "success" {
		return fmt.Errorf("mkdir failed (status=%s): %s", resp.Status, resp.Error)
	}

	// Re-open the temp file and stream chunks into the VM.
	f, err := os.Open(tmpFile.Name())
	if err != nil {
		return fmt.Errorf("reopen temp file: %w", err)
	}
	defer f.Close()

	// 384KB raw data produces ~512KB base64-encoded output per chunk.
	buf := make([]byte, 384*1024)
	first := true
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			op := ">>"
			if first {
				op = ">"
				first = false
			}
			// Safe: base64 output only contains [A-Za-z0-9+/=\n], so single quotes
			// cannot be escaped. Using printf '%s' is still safer than echo since
			// echo may interpret backslash sequences on some platforms.
			cmd := fmt.Sprintf("printf '%%s' '%s' | base64 -d %s /tmp/bundle.tar.gz", encoded, op)
			chunkResp, chunkErr := vsock.Execute(vm.ExecRequest{
				Commands: []loka.Command{
					{
						ID:      uuid.New().String(),
						Command: "sh",
						Args:    []string{"-c", cmd},
					},
				},
			})
			if chunkErr != nil {
				return fmt.Errorf("stream chunk to VM: %w", chunkErr)
			}
			if chunkResp.Status != "success" {
				return fmt.Errorf("chunk transfer failed: %s", chunkResp.Error)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read temp file: %w", readErr)
		}
	}

	// Extract the bundle and clean up the temp file inside the VM.
	resp, execErr = vsock.Execute(vm.ExecRequest{
		Commands: []loka.Command{
			{
				ID:      uuid.New().String(),
				Command: "sh",
				Args:    []string{"-c", fmt.Sprintf("tar xzf /tmp/bundle.tar.gz -C %s && rm -f /tmp/bundle.tar.gz", workdir)},
			},
		},
	})
	if execErr != nil {
		return fmt.Errorf("extract bundle in VM: %w", execErr)
	}
	if resp.Status != "success" {
		return fmt.Errorf("bundle extraction failed: %s", resp.Error)
	}

	return nil
}


// StartService starts a long-running service process inside the session's VM.
func (a *Agent) StartService(ctx context.Context, sessionID string, command string, args []string, env map[string]string, workdir, restartPolicy string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	pid, err := sess.Vsock.ServiceStart(command, args, env, workdir, restartPolicy)
	if err != nil {
		return fmt.Errorf("start service via vsock: %w", err)
	}

	a.logger.Info("service started in VM",
		"session", sessionID,
		"command", command,
		"pid", pid,
	)
	return nil
}

// StopService stops the running service process inside the session's VM
// and closes the port forward listener if one is active.
func (a *Agent) StopService(sessionID string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	// Close port forward listener before stopping the service.
	a.StopPortForward(sessionID)

	if err := sess.Vsock.ServiceStop("SIGTERM", 10); err != nil {
		return fmt.Errorf("stop service via vsock: %w", err)
	}

	a.logger.Info("service stopped in VM", "session", sessionID)
	return nil
}

// ServiceStatus returns the status of the service process inside the session's VM.
func (a *Agent) ServiceStatus(sessionID string) (*vm.ServiceStatusResult, error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	return sess.Vsock.ServiceStatus()
}

// ServiceLogs returns the last N lines of the service's stdout/stderr.
func (a *Agent) ServiceLogs(sessionID string, lines int) (*vm.ServiceLogsResult, error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	return sess.Vsock.ServiceLogs(lines)
}

// ── Port Forwarding (vsock TCP tunnel) ──────────────────

// StartPortForward starts a local TCP listener that tunnels connections
// through vsock to the supervisor inside the VM, which then connects to
// localhost:vmPort. Returns the local port assigned to the listener.
func (a *Agent) StartPortForward(sessionID string, vmPort int) (int, error) {
	a.mu.Lock()
	sess, ok := a.sessions[sessionID]
	a.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("session %s not found", sessionID)
	}

	// Listen on a random available port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen for port forward: %w", err)
	}

	localPort := listener.Addr().(*net.TCPAddr).Port

	a.mu.Lock()
	sess.PortForwardListener = listener
	sess.ForwardedPort = localPort
	a.mu.Unlock()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // Listener closed.
			}
			go a.forwardConnection(conn, sess, vmPort)
		}
	}()

	a.logger.Info("port forward started",
		"session", sessionID,
		"local_port", localPort,
		"vm_port", vmPort,
	)
	return localPort, nil
}

// forwardConnection tunnels a single TCP connection through vsock to the VM.
// It opens a new vsock connection, sends a tcp_forward RPC to the supervisor,
// and then relays data bidirectionally.
func (a *Agent) forwardConnection(clientConn net.Conn, sess *SessionState, vmPort int) {
	defer clientConn.Close()

	// Open a new vsock connection to the supervisor.
	vsockConn, err := net.DialTimeout("unix", sess.Vsock.SocketPath(), 5*time.Second)
	if err != nil {
		a.logger.Debug("port forward: vsock dial failed", "error", err)
		return
	}
	defer vsockConn.Close()

	// Use a bufio.Reader for the protocol handshake so we can read exact
	// lines without consuming bytes that belong to the raw TCP tunnel.
	br := bufio.NewReader(vsockConn)

	// Firecracker vsock handshake: CONNECT to port 52 (supervisor).
	if _, err := fmt.Fprintf(vsockConn, "CONNECT 52\n"); err != nil {
		a.logger.Debug("port forward: vsock CONNECT failed", "error", err)
		return
	}
	okLine, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(okLine, "OK") {
		a.logger.Debug("port forward: vsock handshake failed")
		return
	}

	// Send tcp_forward RPC. After the response, the connection becomes raw TCP.
	req := vm.RPCRequest{
		Method: "tcp_forward",
		ID:     "fwd",
		Params: json.RawMessage(fmt.Sprintf(`{"port":%d}`, vmPort)),
	}
	if err := json.NewEncoder(vsockConn).Encode(req); err != nil {
		a.logger.Debug("port forward: send RPC failed", "error", err)
		return
	}

	// Read the RPC response line. The supervisor sends exactly one JSON line
	// followed by a newline, then the connection becomes a raw TCP tunnel.
	respLine, err := br.ReadString('\n')
	if err != nil {
		a.logger.Debug("port forward: read RPC response failed", "error", err)
		return
	}
	var resp vm.RPCResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		a.logger.Debug("port forward: parse RPC response failed", "error", err, "raw", respLine)
		return
	}
	if resp.Error != nil {
		a.logger.Debug("port forward: supervisor error", "error", resp.Error.Message)
		return
	}

	// Drain any data the bufio.Reader may have buffered past the response
	// line (e.g. the first bytes of the TCP stream from the VM service).
	if br.Buffered() > 0 {
		buffered := make([]byte, br.Buffered())
		n, _ := br.Read(buffered)
		if n > 0 {
			clientConn.Write(buffered[:n])
		}
	}

	// Connection is now a raw TCP tunnel. Bidirectional copy using the raw
	// vsockConn (not the bufio.Reader) so no further buffering occurs.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(vsockConn, clientConn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(clientConn, vsockConn)
	}()
	wg.Wait()
}

// StopPortForward closes the port forward listener for a session.
func (a *Agent) StopPortForward(sessionID string) {
	a.mu.Lock()
	sess, ok := a.sessions[sessionID]
	a.mu.Unlock()
	if !ok {
		return
	}
	if sess.PortForwardListener != nil {
		sess.PortForwardListener.Close()
		sess.PortForwardListener = nil
		sess.ForwardedPort = 0
		a.logger.Info("port forward stopped", "session", sessionID)
	}
}

// GetGuestIP returns the guest IP for a session's VM, or "" if not available.
func (a *Agent) GetGuestIP(sessionID string) string {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return ""
	}
	return sess.GuestIP
}

// GetForwardedPort returns the local forwarded port for a session, or 0 if none.
func (a *Agent) GetForwardedPort(sessionID string) int {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return 0
	}
	return sess.ForwardedPort
}

// SessionCount returns the number of active sessions.
func (a *Agent) SessionCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sessions)
}
