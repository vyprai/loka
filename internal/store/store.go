package store

import (
	"context"
	"time"

	"github.com/vyprai/loka/internal/loka"
)

// Store is the top-level interface for all data access.
// Implementations: SQLite (dev), PostgreSQL (production).
type Store interface {
	Sessions() SessionRepository
	Executions() ExecutionRepository
	Checkpoints() CheckpointRepository
	Workers() WorkerRepository
	Tokens() TokenRepository
	Services() ServiceRepository
	Volumes() VolumeRepository
	Tasks() TaskRepository
	Migrate(ctx context.Context) error
	Close() error
}

// SessionFilter controls session list queries.
type SessionFilter struct {
	Status   *loka.SessionStatus
	WorkerID *string
	Name     *string
	Limit    int
	Offset   int
}

// SessionRepository manages session persistence.
type SessionRepository interface {
	Create(ctx context.Context, session *loka.Session) error
	Get(ctx context.Context, id string) (*loka.Session, error)
	Update(ctx context.Context, session *loka.Session) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, filter SessionFilter) ([]*loka.Session, error)
	ListByWorker(ctx context.Context, workerID string) ([]*loka.Session, error)
	DeleteTerminatedBefore(ctx context.Context, before time.Time) (int, error)
}

// ExecutionFilter controls execution list queries.
type ExecutionFilter struct {
	SessionID *string
	Status    *loka.ExecStatus
	Limit     int
	Offset    int
}

// ExecutionRepository manages execution persistence.
type ExecutionRepository interface {
	Create(ctx context.Context, exec *loka.Execution) error
	Get(ctx context.Context, id string) (*loka.Execution, error)
	Update(ctx context.Context, exec *loka.Execution) error
	ListBySession(ctx context.Context, sessionID string, filter ExecutionFilter) ([]*loka.Execution, error)
	DeleteCompletedBefore(ctx context.Context, before time.Time) (int, error)
}

// CheckpointRepository manages checkpoint persistence.
type CheckpointRepository interface {
	Create(ctx context.Context, cp *loka.Checkpoint) error
	Get(ctx context.Context, id string) (*loka.Checkpoint, error)
	Update(ctx context.Context, cp *loka.Checkpoint) error
	GetDAG(ctx context.Context, sessionID string) (*loka.CheckpointDAG, error)
	ListBySession(ctx context.Context, sessionID string) ([]*loka.Checkpoint, error)
	Delete(ctx context.Context, id string) error
	// DeleteSubtree deletes a checkpoint and all its descendants.
	DeleteSubtree(ctx context.Context, id string) error
}

// WorkerFilter controls worker list queries.
type WorkerFilter struct {
	Provider *string
	Status   *loka.WorkerStatus
	Region   *string
	Labels   map[string]string
	Limit    int
	Offset   int
}

// WorkerRepository manages worker persistence.
type WorkerRepository interface {
	Create(ctx context.Context, w *loka.Worker) error
	Get(ctx context.Context, id string) (*loka.Worker, error)
	Update(ctx context.Context, w *loka.Worker) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, filter WorkerFilter) ([]*loka.Worker, error)
	UpdateHeartbeat(ctx context.Context, id string, hb *loka.Heartbeat) error
}

// ServiceFilter controls service list queries.
type ServiceFilter struct {
	Status     *loka.ServiceStatus
	WorkerID   *string
	Name       *string
	IsDatabase *bool   // If non-nil, filter by database_config presence.
	PrimaryID       *string // If non-nil, filter replicas by primary_id in database_config (JSON).
	ParentServiceID *string // If non-nil, filter by parent_service_id column.
	Limit      int
	Offset     int
}

// IdleCandidate is a lightweight view of a service for idle monitoring.
type IdleCandidate struct {
	ID           string
	Name         string
	IdleTimeout  int
	LastActivity time.Time
}

// ServiceRepository manages service persistence.
type ServiceRepository interface {
	Create(ctx context.Context, svc *loka.Service) error
	Get(ctx context.Context, id string) (*loka.Service, error)
	Update(ctx context.Context, svc *loka.Service) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, filter ServiceFilter) ([]*loka.Service, int, error)
	ListByWorker(ctx context.Context, workerID string) ([]*loka.Service, error)
	ListIdleCandidates(ctx context.Context) ([]IdleCandidate, error)
}

// TokenRepository manages worker registration tokens.
type TokenRepository interface {
	Create(ctx context.Context, token *loka.WorkerToken) error
	Get(ctx context.Context, id string) (*loka.WorkerToken, error)
	GetByToken(ctx context.Context, token string) (*loka.WorkerToken, error)
	MarkUsed(ctx context.Context, id, workerID string) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*loka.WorkerToken, error)
	DeleteExpiredBefore(ctx context.Context, before time.Time) (int, error)
}

// VolumeRepository manages named volume persistence.
type VolumeRepository interface {
	Create(ctx context.Context, vol *loka.VolumeRecord) error
	Get(ctx context.Context, name string) (*loka.VolumeRecord, error)
	Update(ctx context.Context, vol *loka.VolumeRecord) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]*loka.VolumeRecord, error)
	// ListByWorker returns volumes where the given worker is primary or a replica.
	ListByWorker(ctx context.Context, workerID string) ([]*loka.VolumeRecord, error)
	// UpdatePlacement sets the primary and replica worker assignments.
	UpdatePlacement(ctx context.Context, name, primaryWorkerID string, replicaWorkerIDs []string) error
	// UpdateStatus sets the replication status of a volume.
	UpdateStatus(ctx context.Context, name string, status loka.VolumeStatus) error
	// IncrementMountCount atomically increments mount_count by 1.
	IncrementMountCount(ctx context.Context, name string) error
	// DecrementMountCount atomically decrements mount_count by 1 (clamped at 0).
	DecrementMountCount(ctx context.Context, name string) error
}

// TaskFilter controls task list queries.
type TaskFilter struct {
	Status *loka.TaskStatus
	Name   *string
	Limit  int
	Offset int
}

// TaskRepository manages task persistence.
type TaskRepository interface {
	Create(ctx context.Context, task *loka.Task) error
	Get(ctx context.Context, id string) (*loka.Task, error)
	Update(ctx context.Context, task *loka.Task) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, filter TaskFilter) ([]*loka.Task, error)
}
