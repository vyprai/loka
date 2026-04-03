package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/metrics/recorder"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/metrics"
	"github.com/vyprai/loka/internal/store"
)

// WorkerConn represents a connected worker with its command channel.
type WorkerConn struct {
	Worker       *loka.Worker
	CmdChan      chan WorkerCommand
	SessionCount int               // Updated from heartbeat.
	Usage        loka.ResourceUsage // Updated from heartbeat.
}

// WorkerCommand is a command sent from CP to a worker.
type WorkerCommand struct {
	ID   string
	Type string // "launch_session", "stop_session", "exec", "checkpoint", "drain", "update_routes", etc.
	Data any
}

// ServiceExecData is the payload for executing a command inside a running service VM.
type ServiceExecData struct {
	ServiceID string
	Commands  []loka.Command
}

// UpdateRoutesData is the payload for pushing the service route table to a worker.
type UpdateRoutesData struct {
	Version  int64          `json:"version"`  // Route table version for staleness detection.
	Services []ServiceRoute `json:"services"` // All routable services.
}

// ServiceRoute describes a service's network location for worker-to-worker routing.
type ServiceRoute struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	WorkerIP string `json:"worker_ip"` // PrivateIP of the worker hosting this service.
	Port     int    `json:"port"`      // ForwardPort on that worker.
	Engine   string `json:"engine"`    // "postgres", "mysql", "redis" (for DB proxy).
	Role     string `json:"role"`      // "primary", "replica", "component"
}

// LaunchSessionData is the payload for launching a session on a worker.
type LaunchSessionData struct {
	SessionID           string
	ImageRef            string
	Mode                loka.ExecMode
	ExecPolicy          loka.ExecPolicy
	VCPUs               int
	MemoryMB            int
	RootfsPath          string // Image rootfs.
	SnapshotMemPath     string // Warm snapshot memory (for instant restore).
	SnapshotVMStatePath string // Warm snapshot VM state.
	Mounts              []loka.Volume // Volume mounts for the session.
}

// StopSessionData is the payload for stopping/suspending a session.
type StopSessionData struct {
	SessionID string
}

// ExecCommandData is the payload for executing commands in a session.
type ExecCommandData struct {
	SessionID string
	ExecID    string
	Commands  []loka.Command
	Parallel  bool
}

// ShellRelay bridges the CP gRPC shell stream and the worker's vsock PTY tunnel.
// The CP creates this, passes it in a ShellStartData command, and both sides
// use the channels for bidirectional data relay.
type ShellRelay struct {
	Input  chan ShellFrame // CP → worker (stdin data, resize events).
	Output chan ShellFrame // Worker → CP (stdout data, exit event).
	ErrCh  chan error      // Worker signals setup success (nil) or failure.
}

// ShellFrame matches vm.ShellFrame but is duplicated here to avoid importing
// the worker/vm package in the controlplane package.
type ShellFrame struct {
	Type byte
	Data []byte
}

// Shell frame type constants (must match vm.FrameData/FrameResize/FrameExit).
const (
	FrameData   byte = 0x01
	FrameResize byte = 0x02
	FrameExit   byte = 0x03
)

// NewShellRelay creates a ShellRelay with buffered channels.
func NewShellRelay() *ShellRelay {
	return &ShellRelay{
		Input:  make(chan ShellFrame, 64),
		Output: make(chan ShellFrame, 64),
		ErrCh:  make(chan error, 1),
	}
}

// ShellStartData is the payload for the "shell_start" worker command.
type ShellStartData struct {
	SessionID string
	Command   string
	Rows      uint16
	Cols      uint16
	Workdir   string
	Env       map[string]string
	Relay     *ShellRelay
}

// OnWorkerJoinFunc is called when a new worker registers (e.g., to heal degraded volumes).
type OnWorkerJoinFunc func(ctx context.Context)

// Registry manages connected workers.
type Registry struct {
	mu            sync.RWMutex
	workers       map[string]*WorkerConn
	store         store.Store
	logger        *slog.Logger
	recorder      recorder.Recorder
	onWorkerJoin  OnWorkerJoinFunc
}

// NewRegistry creates a new worker registry.
func NewRegistry(s store.Store, logger *slog.Logger, rec recorder.Recorder) *Registry {
	if rec == nil {
		rec = recorder.NopRecorder{}
	}
	return &Registry{
		workers:  make(map[string]*WorkerConn),
		store:    s,
		logger:   logger,
		recorder: rec,
	}
}

// Register adds a new worker to the registry.
func (r *Registry) Register(ctx context.Context, hostname, ipAddr, provider, region, zone, agentVersion string, capacity loka.ResourceCapacity, labels map[string]string, kvmAvailable bool) (*loka.Worker, error) {
	now := time.Now()
	w := &loka.Worker{
		ID:           uuid.New().String(),
		Hostname:     hostname,
		IPAddress:    ipAddr,
		Provider:     provider,
		Region:       region,
		Zone:         zone,
		Status:       loka.WorkerStatusReady,
		Labels:       labels,
		Capacity:     capacity,
		AgentVersion: agentVersion,
		KVMAvailable: kvmAvailable,
		CreatedAt:    now,
		UpdatedAt:    now,
		LastSeen:     now,
	}

	if err := r.store.Workers().Create(ctx, w); err != nil {
		return nil, fmt.Errorf("register worker: %w", err)
	}

	r.mu.Lock()
	r.workers[w.ID] = &WorkerConn{
		Worker:  w,
		CmdChan: make(chan WorkerCommand, 256),
	}
	r.mu.Unlock()

	r.recorder.Inc("worker_registrations_total", metrics.Label{Name: "worker_id", Value: w.ID}, metrics.Label{Name: "hostname", Value: hostname}, metrics.Label{Name: "provider", Value: provider})
	r.logger.Info("worker registered", "id", w.ID, "hostname", hostname, "provider", provider)

	// Notify listeners (e.g., volume manager can heal degraded volumes).
	if r.onWorkerJoin != nil {
		go r.onWorkerJoin(ctx)
	}

	return w, nil
}

// SetOnWorkerJoin sets a callback invoked when a new worker registers.
func (r *Registry) SetOnWorkerJoin(fn OnWorkerJoinFunc) {
	r.onWorkerJoin = fn
}

// Unregister removes a worker from the registry.
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	if conn, ok := r.workers[id]; ok {
		close(conn.CmdChan)
		delete(r.workers, id)
	}
	r.mu.Unlock()
	r.logger.Info("worker unregistered", "id", id)
}

// CleanupDead removes workers in dead status from the in-memory registry.
// Should be called periodically to prevent stale entries from accumulating.
func (r *Registry) CleanupDead() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := 0
	for id, conn := range r.workers {
		if conn.Worker.Status == loka.WorkerStatusDead {
			close(conn.CmdChan)
			delete(r.workers, id)
			count++
		}
	}
	if count > 0 {
		r.logger.Info("cleaned up dead workers from registry", "count", count)
	}
	return count
}

// Get returns a connected worker by ID.
func (r *Registry) Get(id string) (*WorkerConn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conn, ok := r.workers[id]
	return conn, ok
}

// List returns all connected workers.
func (r *Registry) List() []*WorkerConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conns := make([]*WorkerConn, 0, len(r.workers))
	for _, c := range r.workers {
		conns = append(conns, c)
	}
	return conns
}

// ListHealthy returns IDs of all healthy (ready or busy) workers.
func (r *Registry) ListHealthy(_ context.Context) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var ids []string
	for _, c := range r.workers {
		if c.Worker.Status == loka.WorkerStatusReady || c.Worker.Status == loka.WorkerStatusBusy {
			ids = append(ids, c.Worker.ID)
		}
	}
	return ids, nil
}

// SendCommand sends a command to a specific worker.
// Critical commands (stop_session, drain) use a blocking send with timeout
// to avoid dropping them when the channel is full.
func (r *Registry) SendCommand(workerID string, cmd WorkerCommand) error {
	r.mu.RLock()
	conn, ok := r.workers[workerID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("worker %s not connected", workerID)
	}

	// Log warning when channel is >75% full.
	chanLen := len(conn.CmdChan)
	chanCap := cap(conn.CmdChan)
	if chanCap > 0 && chanLen > chanCap*3/4 {
		r.logger.Warn("worker command channel nearly full",
			"worker", workerID,
			"usage", fmt.Sprintf("%d/%d", chanLen, chanCap),
		)
	}

	// Critical commands must not be dropped — use blocking send with timeout.
	if isCriticalCommand(cmd.Type) {
		select {
		case conn.CmdChan <- cmd:
			return nil
		case <-time.After(10 * time.Second):
			return fmt.Errorf("worker %s command channel full, critical command %s timed out", workerID, cmd.Type)
		}
	}

	select {
	case conn.CmdChan <- cmd:
		return nil
	default:
		return fmt.Errorf("worker %s command channel full (%d/%d)", workerID, chanLen, chanCap)
	}
}

// isCriticalCommand returns true for commands that should never be silently dropped.
func isCriticalCommand(cmdType string) bool {
	switch cmdType {
	case "stop_session", "drain", "checkpoint":
		return true
	}
	return false
}

// UpdateHeartbeat updates a worker's last seen time and resource usage.
func (r *Registry) UpdateHeartbeat(ctx context.Context, workerID string, hb *loka.Heartbeat) error {
	r.mu.RLock()
	conn, ok := r.workers[workerID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("worker %s not connected", workerID)
	}

	conn.Worker.LastSeen = hb.Timestamp
	conn.Worker.Status = hb.Status
	conn.SessionCount = hb.SessionCount
	conn.Usage = hb.Usage

	return r.store.Workers().UpdateHeartbeat(ctx, workerID, hb)
}

// PickWorker selects a worker for a new session (basic round-robin for MVP).
func (r *Registry) PickWorker() (*WorkerConn, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, conn := range r.workers {
		if conn.Worker.Status == loka.WorkerStatusReady {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("no available workers")
}
