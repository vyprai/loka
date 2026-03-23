package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// WorkerConn represents a connected worker with its command channel.
type WorkerConn struct {
	Worker  *loka.Worker
	CmdChan chan WorkerCommand
}

// WorkerCommand is a command sent from CP to a worker.
type WorkerCommand struct {
	ID   string
	Type string // "launch_session", "stop_session", "exec", "checkpoint", "drain", etc.
	Data any
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

// Registry manages connected workers.
type Registry struct {
	mu      sync.RWMutex
	workers map[string]*WorkerConn
	store   store.Store
	logger  *slog.Logger
}

// NewRegistry creates a new worker registry.
func NewRegistry(s store.Store, logger *slog.Logger) *Registry {
	return &Registry{
		workers: make(map[string]*WorkerConn),
		store:   s,
		logger:  logger,
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
		CmdChan: make(chan WorkerCommand, 32),
	}
	r.mu.Unlock()

	r.logger.Info("worker registered", "id", w.ID, "hostname", hostname, "provider", provider)
	return w, nil
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

// SendCommand sends a command to a specific worker.
func (r *Registry) SendCommand(workerID string, cmd WorkerCommand) error {
	r.mu.RLock()
	conn, ok := r.workers[workerID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("worker %s not connected", workerID)
	}
	select {
	case conn.CmdChan <- cmd:
		return nil
	default:
		return fmt.Errorf("worker %s command channel full", workerID)
	}
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
