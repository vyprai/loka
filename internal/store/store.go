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
	Status   *loka.ServiceStatus
	WorkerID *string
	Name     *string
	Limit    int
	Offset   int
}

// ServiceRepository manages service persistence.
type ServiceRepository interface {
	Create(ctx context.Context, svc *loka.Service) error
	Get(ctx context.Context, id string) (*loka.Service, error)
	Update(ctx context.Context, svc *loka.Service) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, filter ServiceFilter) ([]*loka.Service, int, error)
	ListByWorker(ctx context.Context, workerID string) ([]*loka.Service, error)
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
