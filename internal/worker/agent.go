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
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/worker/vm"
	"github.com/vyprai/loka/internal/worker/volsync"
	"github.com/vyprai/loka/pkg/lokavm"
	"github.com/vyprai/loka/pkg/lokavm/gitcache"
)

// Agent is the worker-side agent that manages microVMs.
// Uses lokavm.Hypervisor (Apple VZ on macOS, Go KVM on Linux).
// Falls back to lokavm vm.Manager if hypervisor is not set.
type Agent struct {
	id         string
	hostname   string
	provider   string
	dataDir    string
	labels     map[string]string
	logger     *slog.Logger
	remoteMode bool // True when running as a remote worker (not embedded in CP).

	mu            sync.RWMutex
	sessions      map[string]*SessionState
	overlay       *vm.OverlayManager
	checkpointMgr *CheckpointManager
	hypervisor    lokavm.Hypervisor
	objStore      objstore.ObjectStore
	gitCache      *gitcache.Cache
	syncAgent     *volsync.Agent

	// Route table: service name → route (pushed by CP via update_routes command).
	routeTable   sync.Map // string → worker.ServiceRoute
	routeVersion int64    // Atomic: version from last CP push.

	// Warm boot cache: image ref → snapshot for instant VM restore.
	warmMu    sync.RWMutex
	warmCache map[string]*warmSnapshot
}

type warmSnapshot struct {
	imageRef string
	snapshot lokavm.Snapshot
	vmID     string    // The paused VM ID to clone from.
	lastUsed time.Time // For LRU eviction.
}

// ServiceLaunchOpts holds options for launching a service VM.
type ServiceLaunchOpts struct {
	ImageRef            string
	VCPUs               int
	MemoryMB            int
	RootfsPath          string
	LayerPackPath       string // Path to read-only layer-pack ext4.
	Command             string
	Args                []string
	Env                 map[string]string
	Workdir             string
	Port                int
	BundleKey           string
	RestartPolicy       string
	Mounts              []loka.Volume // Volume mounts (FUSE or block mode).
	SnapshotMemPath     string // Warm snapshot memory file for instant restore.
	SnapshotVMStatePath string // Warm snapshot VM state file.
	IsAppSnapshotRestore bool  // If true, skip bundle extraction and service_start (app already running).
	HealthPath          string // HTTP path for health check (empty = TCP only).
}

// SessionState tracks a running session backed by a microVM.
type SessionState struct {
	ID       string
	Mode     loka.ExecMode
	Policy   loka.ExecPolicy
	VM       *lokavm.VM       // lokavm VM instance.
	Vsock    *vm.VsockClient  // vsock connection to supervisor inside VM.
	LayerMap map[string]string

	// Port forwarding: local TCP listener that tunnels to VM via vsock.
	PortForwardListener net.Listener
	ForwardedPort       int // Local port assigned to the port forward listener.

	// App-level warm snapshot paths (per-service, includes running app).
	AppSnapshotMem   string
	AppSnapshotState string

	// opMu protects concurrent operations on this session (exec, stop, etc.)
	// to prevent use-after-free on the vsock connection.
	opMu sync.Mutex
}

// NewAgent creates a new worker agent with lokavm hypervisor.
func NewAgent(provider string, labels map[string]string, dataDir string, objStore objstore.ObjectStore, hypervisor lokavm.Hypervisor, logger *slog.Logger) (*Agent, error) {
	hostname, _ := os.Hostname()
	overlay := vm.NewOverlayManager(dataDir)
	cpMgr := NewCheckpointManager(overlay, objStore, logger)

	gc, err := gitcache.New(gitcache.Config{
		CacheDir: filepath.Join(dataDir, "gitcache"),
		Logger:   logger,
	})
	if err != nil {
		return nil, fmt.Errorf("init git cache: %w", err)
	}

	agent := &Agent{
		id:            uuid.New().String(),
		hostname:      hostname,
		provider:      provider,
		dataDir:       dataDir,
		labels:        labels,
		logger:        logger,
		sessions:      make(map[string]*SessionState),
		overlay:       overlay,
		checkpointMgr: cpMgr,
		hypervisor:    hypervisor,
		objStore:      objStore,
		gitCache:      gc,
		syncAgent:     volsync.NewAgent(dataDir, objStore, logger),
		warmCache:     make(map[string]*warmSnapshot),
	}

	// Start session reaper: removes stale sessions where VM no longer exists.
	go agent.sessionReaper()

	return agent, nil
}

// SetRemoteMode marks the agent as running remotely (not embedded in CP).
// When remote, port forwarding binds to 0.0.0.0 instead of localhost.
func (a *Agent) SetRemoteMode(remote bool) {
	a.remoteMode = remote
}

// HandleUpdateRoutes processes a route table update from the control plane.
func (a *Agent) HandleUpdateRoutes(version int64, services []ServiceRouteEntry) {
	atomic.StoreInt64(&a.routeVersion, version)
	for _, svc := range services {
		a.routeTable.Store(svc.Name, svc)
	}
	a.logger.Info("route table updated", "version", version, "services", len(services))
}

// LookupRoute finds a service route by name from the local cache.
func (a *Agent) LookupRoute(name string) (ServiceRouteEntry, bool) {
	v, ok := a.routeTable.Load(name)
	if !ok {
		return ServiceRouteEntry{}, false
	}
	return v.(ServiceRouteEntry), true
}

// RouteVersion returns the current route table version.
func (a *Agent) RouteVersion() int64 {
	return atomic.LoadInt64(&a.routeVersion)
}

// ServiceRouteEntry is the worker-side view of a service route.
type ServiceRouteEntry struct {
	ID       string
	Name     string
	WorkerIP string
	Port     int
	Engine   string
	Role     string
}

// sessionReaper periodically removes stale session entries where the VM is gone.
func (a *Agent) sessionReaper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if a.hypervisor == nil {
			continue
		}
		vms, err := a.hypervisor.ListVMs()
		if err != nil {
			continue
		}
		activeVMs := make(map[string]bool)
		for _, v := range vms {
			activeVMs[v.ID] = true
		}

		a.mu.Lock()
		for id := range a.sessions {
			if !activeVMs[id] {
				a.logger.Warn("reaping stale session (VM gone)", "session", id)
				delete(a.sessions, id)
			}
		}
		// Warm cache eviction: keep max 10 entries, evict least recently used.
		const maxWarmCache = 10
		for len(a.warmCache) > maxWarmCache {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range a.warmCache {
				if oldestKey == "" || v.lastUsed.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastUsed
				}
			}
			if oldestKey != "" {
				delete(a.warmCache, oldestKey)
			}
		}
		a.mu.Unlock()
	}
}

// ID returns the agent's unique identifier.
func (a *Agent) ID() string { return a.id }

// Hypervisor returns the lokavm hypervisor.
func (a *Agent) Hypervisor() lokavm.Hypervisor { return a.hypervisor }

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
	LayerPackPath       string // Path to read-only layer-pack ext4.
	SnapshotMemPath     string // Warm snapshot memory file for instant restore.
	SnapshotVMStatePath string // Warm snapshot VM state file.
	Mounts              []loka.Volume // Volume mounts for the session.
}

// LaunchSession starts a lokavm microVM for this session.
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

	if a.hypervisor == nil {
		return fmt.Errorf("no hypervisor available")
	}


	// Resolve rootfs: directory (crane-extracted) or ext4 file.
	rootfs := opts.RootfsPath
	if rootfs == "" {
		defaultRootfs := filepath.Join(a.dataDir, "rootfs", "rootfs.ext4")
		if _, err := os.Stat(defaultRootfs); err == nil {
			rootfs = defaultRootfs
		}
	}

	// Detect rootfs type:
	// - colon-separated paths = layered dirs (Docker layers, stacked via overlayfs)
	// - single directory = virtiofs direct
	// - single file = ext4 block device
	isLayered := strings.Contains(rootfs, ":")
	isDir := false
	if !isLayered {
		if info, err := os.Stat(rootfs); err == nil && info.IsDir() {
			isDir = true
		}
	}

	vmCfg := lokavm.VMConfig{
		ID:          sessionID,
		VCPUsMin:    vcpu,
		VCPUsMax:    vcpu,
		MemoryMinMB: mem / 4,
		MemoryMaxMB: mem,
		Vsock:       true,
	}

	if isLayered {
		// Docker-style layered rootfs: each layer is a virtiofs mount (read-only).
		// Initramfs stacks all layers via overlayfs + tmpfs upper (ephemeral).
		// No copies — all VMs share the same layer directories.
		layers := strings.Split(rootfs, ":")
		var layerTags []string
		for i, layerPath := range layers {
			tag := fmt.Sprintf("layer-%d", i)
			vmCfg.SharedDirs = append(vmCfg.SharedDirs, lokavm.SharedDir{
				Tag: tag, HostPath: layerPath, GuestPath: "/", ReadOnly: true,
			})
			layerTags = append(layerTags, tag)
		}
		// Boot args: tell initramfs the number of layers to stack.
		// Each layer is a virtiofs tag "layer-N" (mounted by initramfs).
		vmCfg.BootArgs = fmt.Sprintf("console=hvc0 loka.nlayers=%d", len(layerTags))
	} else if isDir {
		// Single directory rootfs — virtiofs direct (for default rootfs).
		vmCfg.BootArgs = "console=hvc0 rootfstype=virtiofs root=rootfs rw init=/usr/local/bin/loka-supervisor"
		vmCfg.SharedDirs = append(vmCfg.SharedDirs,
			lokavm.SharedDir{Tag: "rootfs", HostPath: rootfs, GuestPath: "/", ReadOnly: false},
		)
	} else if rootfs != "" {
		// ext4 block device.
		vmCfg.BootArgs = "console=hvc0 root=/dev/vda rw init=/usr/local/bin/loka-supervisor"
		vmCfg.Drives = append(vmCfg.Drives, lokavm.Drive{
			ID: "rootfs", Path: rootfs, ReadOnly: false,
		})
	} else {
		vmCfg.BootArgs = "console=hvc0"
	}

	// Resolve provider-specific mounts to local HostPath directories.
	for i := range opts.Mounts {
		if err := a.resolveGitMount(context.Background(), &opts.Mounts[i]); err != nil {
			return fmt.Errorf("resolve mount %d (%s): %w", i, opts.Mounts[i].Provider, err)
		}
		if err := a.resolveVolumeMount(&opts.Mounts[i]); err != nil {
			return fmt.Errorf("resolve mount %d (%s): %w", i, opts.Mounts[i].Provider, err)
		}
	}

	// User-specified shared directories via virtiofs.
	// Each mount is passed as a virtiofs device + kernel arg for the initramfs to mount.
	for i, mount := range opts.Mounts {
		if mount.HostPath != "" {
			tag := fmt.Sprintf("mount-%d", i)
			vmCfg.SharedDirs = append(vmCfg.SharedDirs, lokavm.SharedDir{
				Tag:       tag,
				HostPath:  mount.HostPath,
				GuestPath: mount.Path,
				ReadOnly:  mount.IsReadOnly(),
			})
			vmCfg.BootArgs += fmt.Sprintf(" loka.virtiofs=%s:%s", tag, mount.Path)
		}
	}

	hvVM, err := a.hypervisor.CreateVM(vmCfg)
	if err != nil {
		a.overlay.Cleanup(sessionID)
		return fmt.Errorf("create VM: %w", err)
	}
	if err := a.hypervisor.StartVM(sessionID); err != nil {
		a.hypervisor.StopVM(sessionID) // Best-effort cleanup.
		a.overlay.Cleanup(sessionID)
		return fmt.Errorf("start VM: %w", err)
	}

	vsockClient := vm.NewVsockClientFromDialer(hvVM.DialVsock)

	// Wait for supervisor to be ready — prewarm the vsock connection.
	supervisorReady := false
	backoff := 50 * time.Millisecond
	for i := 0; i < 60; i++ {
		if err := vsockClient.Ping(); err == nil {
			supervisorReady = true
			break
		}
		time.Sleep(backoff)
		if backoff < 1*time.Second {
			backoff = time.Duration(float64(backoff) * 1.3)
		}
	}
	if !supervisorReady {
		// Clean up the VM since the supervisor never came up.
		a.hypervisor.StopVM(sessionID)
		a.overlay.Cleanup(sessionID)
		return fmt.Errorf("supervisor not reachable after 60 retries")
	}

	vsockClient.SetPolicy(opts.Policy)
	vsockClient.SetMode(opts.Mode)

	a.sessions[sessionID] = &SessionState{
		ID:       sessionID,
		Mode:     opts.Mode,
		Policy:   opts.Policy,
		VM:       hvVM,
		Vsock:    vsockClient,
		LayerMap: make(map[string]string),
	}

	a.logger.Info("session launched", "session", sessionID, "mode", opts.Mode)
	return nil
}

// cloneDir creates a copy of src directory at dst.
// On macOS APFS, uses `cp -c` for instant copy-on-write clones.
func cloneDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create clone dir: %w", err)
	}
	// Try APFS clone first (instant, zero disk until writes).
	if err := exec.Command("cp", "-ac", src, dst).Run(); err == nil {
		return nil
	}
	// Fallback: regular copy.
	return exec.Command("cp", "-a", src, dst).Run()
}

// findSupervisorBinary locates the loka-supervisor binary to inject into VMs.
func (a *Agent) findSupervisorBinary() string {
	candidates := []string{
		filepath.Join(a.dataDir, "bin", "loka-supervisor"),
		"/usr/local/bin/loka-supervisor",
	}
	// Check sibling of lokad binary.
	if self, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(self), "loka-supervisor")}, candidates...)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// copyFileSimple copies a file from src to dst.
func copyFileSimple(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (a *Agent) stopVM(id string) {
	if a.hypervisor != nil {
		a.hypervisor.StopVM(id)
	}
	// Clean up per-session rootfs clone.
	os.RemoveAll(filepath.Join(a.dataDir, "vms", id))
}

// resolveGitMount resolves a provider="github" or "git" mount by cloning
// the repo into the local git cache and setting HostPath.
func (a *Agent) resolveGitMount(ctx context.Context, mount *loka.Volume) error {
	if mount.Provider != "github" && mount.Provider != "git" {
		return nil
	}
	if mount.GitRepo == "" {
		return fmt.Errorf("git_repo is required for provider=%q", mount.Provider)
	}

	ref := mount.GitRef
	if ref == "" {
		ref = "HEAD"
	}

	path, sha, err := a.gitCache.Checkout(ctx, mount.GitRepo, ref, mount.Credentials)
	if err != nil {
		return fmt.Errorf("git checkout %s@%s: %w", mount.GitRepo, ref, err)
	}

	mount.HostPath = path
	if mount.Access == "" {
		mount.Access = "readonly"
	}

	a.logger.Info("resolved git mount",
		"repo", mount.GitRepo, "ref", ref, "sha", sha[:12], "path", mount.Path)
	return nil
}

// resolveVolumeMount resolves a provider="volume" or "store" mount by creating
// or locating a local directory on the worker. Volumes are local-first: files
// live on the worker's disk and are shared with VMs via virtiofs for fast access.
// Cross-worker sync happens via the objstore in the background.
func (a *Agent) resolveVolumeMount(mount *loka.Volume) error {
	if mount.Provider != "store" && mount.Provider != "volume" && mount.Provider != "bundle" {
		return nil
	}
	if mount.Name == "" {
		return fmt.Errorf("name is required for volume mount")
	}

	// Bundles are readonly — stored in {dataDir}/../bundles/{name}/.
	// Volumes are readwrite — stored in {dataDir}/../volumes/{name}/.
	var volDir string
	if mount.Provider == "bundle" {
		volDir = filepath.Join(a.dataDir, "..", "bundles", mount.Name)
	} else {
		volDir = filepath.Join(a.dataDir, "..", "volumes", mount.Name)
	}
	if abs, err := filepath.Abs(volDir); err == nil {
		volDir = abs
	}
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		return fmt.Errorf("create volume directory: %w", err)
	}

	// Pull latest from objstore if volume has remote data.
	if a.syncAgent != nil {
		a.syncAgent.SyncFromRemote(mount.Name)
	}

	mount.HostPath = volDir

	// Start watching for changes (syncs writes to objstore).
	if a.syncAgent != nil {
		a.syncAgent.WatchVolume(mount.Name)
	}

	a.logger.Info("resolved volume mount", "volume", mount.Name, "path", volDir)
	return nil
}

// isMounted checks if a path is a mount point by comparing device IDs.

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
		return fmt.Errorf("instance %s not found", sessionID)
	}
	// Close port forward listener if active.
	if sess.PortForwardListener != nil {
		sess.PortForwardListener.Close()
		sess.PortForwardListener = nil
		sess.ForwardedPort = 0
	}
	a.mu.Unlock()

	// Wait for any in-flight operations on this session to complete,
	// then close the vsock connection safely.
	sess.opMu.Lock()
	if sess.Vsock != nil {
		sess.Vsock.Close()
		sess.Vsock = nil
	}
	sess.opMu.Unlock()

	// Stop the VM before removing from sessions map to avoid orphans.
	a.stopVM(sessionID)

	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()

	a.logger.Info("session stopped", "session", sessionID)
	return nil
}

// SetMode changes the execution mode — sends to supervisor via vsock.
func (a *Agent) SetMode(sessionID string, mode loka.ExecMode) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("instance %s not found", sessionID)
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
	if !ok || sess == nil || sess.Vsock == nil {
		return &ExecResult{ExecID: execID, Status: loka.ExecStatusFailed, Error: fmt.Sprintf("instance %s not found", sessionID)}
	}

	// Hold per-session lock during vsock operations to prevent use-after-free
	// if StopSession closes the vsock concurrently.
	sess.opMu.Lock()
	vsock := sess.Vsock
	if vsock == nil {
		sess.opMu.Unlock()
		return &ExecResult{ExecID: execID, Status: loka.ExecStatusFailed, Error: "session stopped"}
	}
	resp, err := vsock.Execute(vm.ExecRequest{Commands: commands, Parallel: parallel, ExecID: execID})
	sess.opMu.Unlock()

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
		return fmt.Errorf("instance %s not found", sessionID)
	}
	return sess.Vsock.ApproveCommand(commandID, addToWhitelist)
}

// DenyOnGate sends deny to the supervisor's gate via vsock.
func (a *Agent) DenyOnGate(sessionID, commandID, reason string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("instance %s not found", sessionID)
	}
	return sess.Vsock.DenyCommand(commandID, reason)
}

// AddToWhitelist permanently whitelists a command via vsock.
func (a *Agent) AddToWhitelist(sessionID, command string) error {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("instance %s not found", sessionID)
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
			Error:        fmt.Sprintf("instance %s not found", sessionID),
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
		return fmt.Errorf("instance %s not found", sessionID)
	}

	return a.checkpointMgr.Restore(ctx, sessionID, checkpointID, overlayKey)
}

// DiffCheckpoints returns filesystem diff between two checkpoint layers.
func (a *Agent) DiffCheckpoints(sessionID, cpIDA, cpIDB string) ([]vm.DiffEntry, error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("instance %s not found", sessionID)
	}

	layerA, okA := sess.LayerMap[cpIDA]
	layerB, okB := sess.LayerMap[cpIDB]
	if !okA || !okB {
		return nil, fmt.Errorf("checkpoint layer mapping not found")
	}

	return a.checkpointMgr.Diff(sessionID, layerA, layerB)
}

// ── Service Process Methods ──────────────────────────────

// LaunchService boots a lokavm VM for a service, optionally extracts a
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

	if a.hypervisor == nil {
		return fmt.Errorf("no hypervisor available")
	}

	rootfs := opts.RootfsPath
	isLayered := strings.Contains(rootfs, ":")
	rootfsIsDir := false
	if !isLayered {
		if info, err := os.Stat(rootfs); err == nil && info.IsDir() {
			rootfsIsDir = true
		}
	}

	vmCfg := lokavm.VMConfig{
		ID:          serviceID,
		VCPUsMin:    vcpu,
		VCPUsMax:    vcpu,
		MemoryMinMB: mem / 4,
		MemoryMaxMB: mem,
		Vsock:       true,
	}

	if isLayered {
		// Docker-style layered rootfs: each layer is a virtiofs mount (read-only).
		layers := strings.Split(rootfs, ":")
		for i, layerPath := range layers {
			tag := fmt.Sprintf("layer-%d", i)
			vmCfg.SharedDirs = append(vmCfg.SharedDirs, lokavm.SharedDir{
				Tag: tag, HostPath: layerPath, GuestPath: "/", ReadOnly: true,
			})
		}
		vmCfg.BootArgs = fmt.Sprintf("console=hvc0 loka.nlayers=%d", len(layers))
	} else if rootfsIsDir {
		vmCfg.BootArgs = "console=hvc0 rootfstype=virtiofs root=rootfs rw init=/usr/local/bin/loka-supervisor"
		vmCfg.SharedDirs = append(vmCfg.SharedDirs, lokavm.SharedDir{
			Tag: "rootfs", HostPath: rootfs, GuestPath: "/", ReadOnly: false,
		})
	} else if rootfs != "" {
		vmCfg.BootArgs = "console=hvc0 root=/dev/vda rw init=/usr/local/bin/loka-supervisor"
		vmCfg.Drives = append(vmCfg.Drives, lokavm.Drive{
			ID: "rootfs", Path: rootfs, ReadOnly: false,
		})
	} else {
		vmCfg.BootArgs = "console=hvc0"
	}

	// Resolve provider-specific mounts to local HostPath directories.
	for i := range opts.Mounts {
		if err := a.resolveVolumeMount(&opts.Mounts[i]); err != nil {
			return fmt.Errorf("resolve mount %d (%s): %w", i, opts.Mounts[i].Provider, err)
		}
	}

	for i, mount := range opts.Mounts {
		if mount.HostPath != "" {
			tag := fmt.Sprintf("mount-%d", i)
			vmCfg.SharedDirs = append(vmCfg.SharedDirs, lokavm.SharedDir{
				Tag:       tag,
				HostPath:  mount.HostPath,
				GuestPath: mount.Path,
				ReadOnly:  mount.IsReadOnly(),
			})
			vmCfg.BootArgs += fmt.Sprintf(" loka.virtiofs=%s:%s", tag, mount.Path)
		}
	}

	hvVM, err := a.hypervisor.CreateVM(vmCfg)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}
	if err := a.hypervisor.StartVM(serviceID); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	vsockClient := vm.NewVsockClientFromDialer(hvVM.DialVsock)

	// Wait for supervisor to be ready.
	backoff := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		if err := vsockClient.Ping(); err == nil {
			break
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}

	if opts.IsAppSnapshotRestore {
		a.mu.Lock()
		a.sessions[serviceID] = &SessionState{
			ID:       serviceID,
			VM:       hvVM,
			Vsock:    vsockClient,
			LayerMap: make(map[string]string),
		}
		a.mu.Unlock()

		if opts.Port > 0 {
			if localPort, err := a.StartPortForward(serviceID, opts.Port); err == nil {
				a.logger.Info("service restored from app snapshot",
					"service", serviceID, "forward_port", localPort)
				return nil
			}
		}
		a.logger.Info("service restored from app snapshot", "service", serviceID)
		return nil
	}

	// Check if /workspace is already mounted as a read-only volume (bundle volume).
	// If so, skip inline bundle extraction — the bundle is already available.
	hasBundleVolume := false
	for _, m := range opts.Mounts {
		if m.Path == "/workspace" {
			hasBundleVolume = true
			break
		}
	}

	// Extract bundle into the VM if a BundleKey is provided and no volume covers /workspace.
	if !hasBundleVolume && opts.BundleKey != "" && a.objStore != nil {
		if err := a.extractBundle(ctx, vsockClient, opts.BundleKey, opts.Workdir); err != nil {
			a.stopVM(serviceID)
			return fmt.Errorf("extract bundle: %w", err)
		}
	}

	// Start the service process inside the VM.
	restartPolicy := opts.RestartPolicy
	if restartPolicy == "" {
		restartPolicy = "on-failure"
	}
	if _, err := vsockClient.ServiceStart(opts.Command, opts.Args, opts.Env, opts.Workdir, restartPolicy); err != nil {
		a.stopVM(serviceID)
		return fmt.Errorf("start service: %w", err)
	}

	// Store the session/VM reference so other service methods can find it.
	a.mu.Lock()
	a.sessions[serviceID] = &SessionState{
		ID:       serviceID,
		VM:       hvVM,
		Vsock:    vsockClient,
		LayerMap: make(map[string]string),
	}
	a.mu.Unlock()

	if opts.Port > 0 {
		localPort, err := a.StartPortForward(serviceID, opts.Port)
		if err != nil {
			a.logger.Warn("port forward failed", "service", serviceID, "error", err)
		} else {
			a.logger.Info("service launched", "service", serviceID, "command", opts.Command, "forward_port", localPort)
		}
	} else {
		a.logger.Info("service launched", "service", serviceID, "command", opts.Command)
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
			if a.hypervisor == nil { return }
			snap, err := a.hypervisor.CreateSnapshot(serviceID)
			if err != nil {
				a.logger.Warn("failed to create app snapshot", "service", serviceID, "error", err)
				return
			}
			if err := a.hypervisor.ResumeVM(serviceID); err != nil {
				a.logger.Warn("failed to resume VM after snapshot", "service", serviceID, "error", err)
			}
			a.logger.Info("app snapshot created", "service", serviceID, "snapshot", snap)
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
		return fmt.Errorf("instance %s not found", sessionID)
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
		return fmt.Errorf("instance %s not found", sessionID)
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
		return nil, fmt.Errorf("instance %s not found", sessionID)
	}

	return sess.Vsock.ServiceStatus()
}

// ServiceLogs returns the last N lines of the service's stdout/stderr.
func (a *Agent) ServiceLogs(sessionID string, lines int) (*vm.ServiceLogsResult, error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("instance %s not found", sessionID)
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
		return 0, fmt.Errorf("instance %s not found", sessionID)
	}

	// Listen on a random available port.
	// Remote workers bind to all interfaces so the CP can reach them.
	bindAddr := "127.0.0.1:0"
	if a.remoteMode {
		bindAddr = "0.0.0.0:0"
	}
	listener, err := net.Listen("tcp", bindAddr)
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
	// Use the VM's DialVsock for lokavm (direct vsock), fall back to UDS for Firecracker.
	var vsockConn net.Conn
	if sess.VM != nil && sess.VM.DialVsock != nil {
		var err error
		vsockConn, err = sess.VM.DialVsock(52)
		if err != nil {
			a.logger.Debug("port forward: vsock dial failed", "error", err)
			return
		}
	} else {
		var err error
		vsockConn, err = net.DialTimeout("unix", sess.Vsock.SocketPath(), 5*time.Second)
		if err != nil {
			a.logger.Debug("port forward: vsock dial failed (UDS)", "error", err)
			return
		}
		// Firecracker UDS needs CONNECT handshake.
		if _, err := fmt.Fprintf(vsockConn, "CONNECT 52\n"); err != nil {
			vsockConn.Close()
			a.logger.Debug("port forward: vsock CONNECT failed", "error", err)
			return
		}
		buf := make([]byte, 32)
		n, err := vsockConn.Read(buf)
		if err != nil || !strings.HasPrefix(string(buf[:n]), "OK") {
			vsockConn.Close()
			a.logger.Debug("port forward: vsock handshake failed")
			return
		}
	}
	defer vsockConn.Close()

	br := bufio.NewReader(vsockConn)

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
	a.logger.Info("port forward: tunnel established", "port", vmPort)

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
	// When either direction finishes (EOF or error), close both connections
	// to unblock the other io.Copy goroutine and prevent leaks.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(vsockConn, clientConn)
		vsockConn.Close() // Unblock the other direction.
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, vsockConn)
		clientConn.Close() // Unblock the other direction.
		done <- struct{}{}
	}()
	<-done
	<-done
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

// GetGuestIP returns the guest IP for a session's VM.
// With lokavm, vsock is used instead of TCP — returns empty.
func (a *Agent) GetGuestIP(sessionID string) string {
	return ""
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

// StartShell opens an interactive PTY shell in a session's VM.
// It dials a raw vsock connection to the supervisor, sends a shell_start RPC,
// and bridges the framed PTY I/O to the relay channels until the shell exits.
func (a *Agent) StartShell(sessionID, command string, rows, cols uint16, workdir string, env map[string]string, inputCh <-chan vm.ShellFrame, outputCh chan<- vm.ShellFrame, errCh chan<- error) {
	a.mu.RLock()
	sess, ok := a.sessions[sessionID]
	a.mu.RUnlock()
	if !ok {
		errCh <- fmt.Errorf("instance %s not found", sessionID)
		return
	}

	if sess.VM == nil || sess.VM.DialVsock == nil {
		errCh <- fmt.Errorf("session %s has no vsock dialer", sessionID)
		return
	}

	// Open a dedicated vsock connection (not the pooled one — shell is long-lived).
	conn, err := sess.VM.DialVsock(52)
	if err != nil {
		errCh <- fmt.Errorf("vsock dial: %w", err)
		return
	}
	defer conn.Close()

	// Send shell_start RPC.
	params, _ := json.Marshal(map[string]interface{}{
		"command": command,
		"rows":    rows,
		"cols":    cols,
		"workdir": workdir,
		"env":     env,
	})
	req := vm.RPCRequest{
		Method: "shell_start",
		ID:     "shell",
		Params: params,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		errCh <- fmt.Errorf("send shell_start: %w", err)
		return
	}

	// Read the RPC response.
	br := bufio.NewReader(conn)
	respLine, err := br.ReadString('\n')
	if err != nil {
		errCh <- fmt.Errorf("read shell_start response: %w", err)
		return
	}
	var resp vm.RPCResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		errCh <- fmt.Errorf("parse shell_start response: %w", err)
		return
	}
	if resp.Error != nil {
		errCh <- fmt.Errorf("supervisor: %s", resp.Error.Message)
		return
	}

	// Drain any buffered data from the bufio.Reader before switching to raw I/O.
	if br.Buffered() > 0 {
		buffered := make([]byte, br.Buffered())
		n, _ := br.Read(buffered)
		if n > 0 {
			// These are the first bytes of framed output — parse and forward them.
			// For simplicity, send as raw data; the frame reader will handle alignment
			// on subsequent reads via the raw conn.
			outputCh <- vm.ShellFrame{Type: vm.FrameData, Data: buffered[:n]}
		}
	}

	// Signal success.
	errCh <- nil

	a.logger.Info("shell connected", "session", sessionID, "command", command)

	// Bridge relay channels ↔ framed vsock I/O.
	var wg sync.WaitGroup

	// done is closed when the shell exits (vsock read returns error or exit frame).
	done := make(chan struct{})

	// Goroutine 1: vsock → outputCh (PTY output and exit).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for {
			frame, err := vm.ReadFrame(conn)
			if err != nil {
				return
			}
			select {
			case outputCh <- frame:
			default:
				return
			}
			if frame.Type == vm.FrameExit {
				return
			}
		}
	}()

	// Goroutine 2: inputCh → vsock (stdin data and resize).
	// Uses done channel to avoid hanging if inputCh is never closed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case frame, ok := <-inputCh:
				if !ok {
					return
				}
				if err := vm.WriteFrame(conn, frame); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	wg.Wait()
	close(outputCh)
	a.logger.Info("shell disconnected", "session", sessionID)
}

// SessionCount returns the number of active sessions.
func (a *Agent) SessionCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.sessions)
}
