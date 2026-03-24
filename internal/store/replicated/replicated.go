// Package replicated provides a store.Store wrapper that replicates all write
// operations through an HA coordinator (Raft consensus). In HA mode with SQLite,
// this ensures all nodes have identical databases.
//
// Architecture:
//   - Write operations (Create, Update, Delete) are serialized to JSON and
//     sent through the coordinator's Apply method (Raft consensus log).
//   - The coordinator's FSM applies each operation on ALL nodes by calling
//     the registered handler, which executes the write on the local store.
//   - Read operations go directly to the local store (fast, eventually consistent
//     with leader — in practice immediately consistent since Raft is synchronous).
//   - Snapshots and restore are handled by the Raft snapshot mechanism.
package replicated

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

const opName = "store_write"

// Coordinator is the subset of ha.Coordinator needed for replication.
type Coordinator interface {
	Apply(ctx context.Context, cmd []byte) (interface{}, error)
	RegisterHandler(op string, fn func(data []byte) interface{})
	IsLeader(name string) bool
}

// storeCmd is the serialized form of a replicated write operation.
type storeCmd struct {
	Op     string          `json:"op"`     // Always "store_write".
	Entity string          `json:"entity"` // "session", "execution", "checkpoint", "worker", "token"
	Action string          `json:"action"` // "create", "update", "delete", etc.
	Data   json.RawMessage `json:"data"`   // Serialized entity or ID.
}

// Store wraps a local store.Store and replicates writes through consensus.
type Store struct {
	local       store.Store
	coordinator Coordinator
	logger      *slog.Logger
}

// New creates a replicated store. It registers a handler on the coordinator
// so that all nodes (including this one) apply writes from the Raft log.
func New(local store.Store, coord Coordinator, logger *slog.Logger) *Store {
	s := &Store{
		local:       local,
		coordinator: coord,
		logger:      logger,
	}

	// Register the FSM handler — called on ALL nodes when a store_write is applied.
	coord.RegisterHandler(opName, func(data []byte) interface{} {
		var cmd storeCmd
		if err := json.Unmarshal(data, &cmd); err != nil {
			return fmt.Sprintf("unmarshal store cmd: %v", err)
		}
		if err := s.applyLocal(cmd); err != nil {
			logger.Error("replicated store apply failed", "entity", cmd.Entity, "action", cmd.Action, "error", err)
			return err.Error()
		}
		return nil
	})

	return s
}

// apply sends a write through consensus and waits for it to be applied.
func (s *Store) apply(ctx context.Context, entity, action string, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal data: %w", err)
	}
	cmd, err := json.Marshal(storeCmd{
		Op:     opName,
		Entity: entity,
		Action: action,
		Data:   payload,
	})
	if err != nil {
		return fmt.Errorf("marshal cmd: %w", err)
	}
	result, err := s.coordinator.Apply(ctx, cmd)
	if err != nil {
		return fmt.Errorf("consensus apply: %w", err)
	}
	if errStr, ok := result.(string); ok && errStr != "" {
		return fmt.Errorf("apply error: %s", errStr)
	}
	return nil
}

// applyLocal executes a replicated write on the local store.
// Called by the FSM handler on ALL nodes.
func (s *Store) applyLocal(cmd storeCmd) error {
	ctx := context.Background()
	switch cmd.Entity {
	case "session":
		return s.applySession(ctx, cmd)
	case "execution":
		return s.applyExecution(ctx, cmd)
	case "checkpoint":
		return s.applyCheckpoint(ctx, cmd)
	case "worker":
		return s.applyWorker(ctx, cmd)
	case "token":
		return s.applyToken(ctx, cmd)
	default:
		return fmt.Errorf("unknown entity: %s", cmd.Entity)
	}
}

// ── Session operations ─────────────────────────────────

func (s *Store) applySession(ctx context.Context, cmd storeCmd) error {
	repo := s.local.Sessions()
	switch cmd.Action {
	case "create":
		var v loka.Session
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Create(ctx, &v)
	case "update":
		var v loka.Session
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Update(ctx, &v)
	case "delete":
		var id string
		if err := json.Unmarshal(cmd.Data, &id); err != nil {
			return err
		}
		return repo.Delete(ctx, id)
	case "delete_terminated_before":
		var t time.Time
		if err := json.Unmarshal(cmd.Data, &t); err != nil {
			return err
		}
		_, err := repo.DeleteTerminatedBefore(ctx, t)
		return err
	default:
		return fmt.Errorf("unknown session action: %s", cmd.Action)
	}
}

// ── Execution operations ───────────────────────────────

func (s *Store) applyExecution(ctx context.Context, cmd storeCmd) error {
	repo := s.local.Executions()
	switch cmd.Action {
	case "create":
		var v loka.Execution
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Create(ctx, &v)
	case "update":
		var v loka.Execution
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Update(ctx, &v)
	case "delete_completed_before":
		var t time.Time
		if err := json.Unmarshal(cmd.Data, &t); err != nil {
			return err
		}
		_, err := repo.DeleteCompletedBefore(ctx, t)
		return err
	default:
		return fmt.Errorf("unknown execution action: %s", cmd.Action)
	}
}

// ── Checkpoint operations ──────────────────────────────

func (s *Store) applyCheckpoint(ctx context.Context, cmd storeCmd) error {
	repo := s.local.Checkpoints()
	switch cmd.Action {
	case "create":
		var v loka.Checkpoint
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Create(ctx, &v)
	case "delete":
		var id string
		if err := json.Unmarshal(cmd.Data, &id); err != nil {
			return err
		}
		return repo.Delete(ctx, id)
	case "delete_subtree":
		var id string
		if err := json.Unmarshal(cmd.Data, &id); err != nil {
			return err
		}
		return repo.DeleteSubtree(ctx, id)
	default:
		return fmt.Errorf("unknown checkpoint action: %s", cmd.Action)
	}
}

// ── Worker operations ──────────────────────────────────

func (s *Store) applyWorker(ctx context.Context, cmd storeCmd) error {
	repo := s.local.Workers()
	switch cmd.Action {
	case "create":
		var v loka.Worker
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Create(ctx, &v)
	case "update":
		var v loka.Worker
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Update(ctx, &v)
	case "delete":
		var id string
		if err := json.Unmarshal(cmd.Data, &id); err != nil {
			return err
		}
		return repo.Delete(ctx, id)
	case "update_heartbeat":
		var p struct {
			ID string          `json:"id"`
			HB *loka.Heartbeat `json:"hb"`
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return repo.UpdateHeartbeat(ctx, p.ID, p.HB)
	default:
		return fmt.Errorf("unknown worker action: %s", cmd.Action)
	}
}

// ── Token operations ───────────────────────────────────

func (s *Store) applyToken(ctx context.Context, cmd storeCmd) error {
	repo := s.local.Tokens()
	switch cmd.Action {
	case "create":
		var v loka.WorkerToken
		if err := json.Unmarshal(cmd.Data, &v); err != nil {
			return err
		}
		return repo.Create(ctx, &v)
	case "delete":
		var id string
		if err := json.Unmarshal(cmd.Data, &id); err != nil {
			return err
		}
		return repo.Delete(ctx, id)
	case "mark_used":
		var p struct {
			ID       string `json:"id"`
			WorkerID string `json:"worker_id"`
		}
		if err := json.Unmarshal(cmd.Data, &p); err != nil {
			return err
		}
		return repo.MarkUsed(ctx, p.ID, p.WorkerID)
	case "delete_expired_before":
		var t time.Time
		if err := json.Unmarshal(cmd.Data, &t); err != nil {
			return err
		}
		_, err := repo.DeleteExpiredBefore(ctx, t)
		return err
	default:
		return fmt.Errorf("unknown token action: %s", cmd.Action)
	}
}

// ── store.Store interface ──────────────────────────────

func (s *Store) Sessions() store.SessionRepository    { return &replicatedSessionRepo{s: s} }
func (s *Store) Executions() store.ExecutionRepository { return &replicatedExecutionRepo{s: s} }
func (s *Store) Checkpoints() store.CheckpointRepository { return &replicatedCheckpointRepo{s: s} }
func (s *Store) Workers() store.WorkerRepository      { return &replicatedWorkerRepo{s: s} }
func (s *Store) Tokens() store.TokenRepository        { return &replicatedTokenRepo{s: s} }
func (s *Store) Migrate(ctx context.Context) error     { return s.local.Migrate(ctx) }
func (s *Store) Close() error                          { return s.local.Close() }

// ── Replicated SessionRepository ───────────────────────

type replicatedSessionRepo struct{ s *Store }

func (r *replicatedSessionRepo) Create(ctx context.Context, session *loka.Session) error {
	return r.s.apply(ctx, "session", "create", session)
}
func (r *replicatedSessionRepo) Get(ctx context.Context, id string) (*loka.Session, error) {
	return r.s.local.Sessions().Get(ctx, id)
}
func (r *replicatedSessionRepo) Update(ctx context.Context, session *loka.Session) error {
	return r.s.apply(ctx, "session", "update", session)
}
func (r *replicatedSessionRepo) Delete(ctx context.Context, id string) error {
	return r.s.apply(ctx, "session", "delete", id)
}
func (r *replicatedSessionRepo) List(ctx context.Context, filter store.SessionFilter) ([]*loka.Session, error) {
	return r.s.local.Sessions().List(ctx, filter)
}
func (r *replicatedSessionRepo) ListByWorker(ctx context.Context, workerID string) ([]*loka.Session, error) {
	return r.s.local.Sessions().ListByWorker(ctx, workerID)
}
func (r *replicatedSessionRepo) DeleteTerminatedBefore(ctx context.Context, before time.Time) (int, error) {
	if err := r.s.apply(ctx, "session", "delete_terminated_before", before); err != nil {
		return 0, err
	}
	return 0, nil // Count not available through consensus.
}

// ── Replicated ExecutionRepository ─────────────────────

type replicatedExecutionRepo struct{ s *Store }

func (r *replicatedExecutionRepo) Create(ctx context.Context, exec *loka.Execution) error {
	return r.s.apply(ctx, "execution", "create", exec)
}
func (r *replicatedExecutionRepo) Get(ctx context.Context, id string) (*loka.Execution, error) {
	return r.s.local.Executions().Get(ctx, id)
}
func (r *replicatedExecutionRepo) Update(ctx context.Context, exec *loka.Execution) error {
	return r.s.apply(ctx, "execution", "update", exec)
}
func (r *replicatedExecutionRepo) ListBySession(ctx context.Context, sessionID string, filter store.ExecutionFilter) ([]*loka.Execution, error) {
	return r.s.local.Executions().ListBySession(ctx, sessionID, filter)
}
func (r *replicatedExecutionRepo) DeleteCompletedBefore(ctx context.Context, before time.Time) (int, error) {
	if err := r.s.apply(ctx, "execution", "delete_completed_before", before); err != nil {
		return 0, err
	}
	return 0, nil
}

// ── Replicated CheckpointRepository ────────────────────

type replicatedCheckpointRepo struct{ s *Store }

func (r *replicatedCheckpointRepo) Create(ctx context.Context, cp *loka.Checkpoint) error {
	return r.s.apply(ctx, "checkpoint", "create", cp)
}
func (r *replicatedCheckpointRepo) Get(ctx context.Context, id string) (*loka.Checkpoint, error) {
	return r.s.local.Checkpoints().Get(ctx, id)
}
func (r *replicatedCheckpointRepo) GetDAG(ctx context.Context, sessionID string) (*loka.CheckpointDAG, error) {
	return r.s.local.Checkpoints().GetDAG(ctx, sessionID)
}
func (r *replicatedCheckpointRepo) ListBySession(ctx context.Context, sessionID string) ([]*loka.Checkpoint, error) {
	return r.s.local.Checkpoints().ListBySession(ctx, sessionID)
}
func (r *replicatedCheckpointRepo) Delete(ctx context.Context, id string) error {
	return r.s.apply(ctx, "checkpoint", "delete", id)
}
func (r *replicatedCheckpointRepo) DeleteSubtree(ctx context.Context, id string) error {
	return r.s.apply(ctx, "checkpoint", "delete_subtree", id)
}

// ── Replicated WorkerRepository ────────────────────────

type replicatedWorkerRepo struct{ s *Store }

func (r *replicatedWorkerRepo) Create(ctx context.Context, w *loka.Worker) error {
	return r.s.apply(ctx, "worker", "create", w)
}
func (r *replicatedWorkerRepo) Get(ctx context.Context, id string) (*loka.Worker, error) {
	return r.s.local.Workers().Get(ctx, id)
}
func (r *replicatedWorkerRepo) Update(ctx context.Context, w *loka.Worker) error {
	return r.s.apply(ctx, "worker", "update", w)
}
func (r *replicatedWorkerRepo) Delete(ctx context.Context, id string) error {
	return r.s.apply(ctx, "worker", "delete", id)
}
func (r *replicatedWorkerRepo) List(ctx context.Context, filter store.WorkerFilter) ([]*loka.Worker, error) {
	return r.s.local.Workers().List(ctx, filter)
}
func (r *replicatedWorkerRepo) UpdateHeartbeat(ctx context.Context, id string, hb *loka.Heartbeat) error {
	return r.s.apply(ctx, "worker", "update_heartbeat", struct {
		ID string          `json:"id"`
		HB *loka.Heartbeat `json:"hb"`
	}{id, hb})
}

// ── Replicated TokenRepository ─────────────────────────

type replicatedTokenRepo struct{ s *Store }

func (r *replicatedTokenRepo) Create(ctx context.Context, token *loka.WorkerToken) error {
	return r.s.apply(ctx, "token", "create", token)
}
func (r *replicatedTokenRepo) Get(ctx context.Context, id string) (*loka.WorkerToken, error) {
	return r.s.local.Tokens().Get(ctx, id)
}
func (r *replicatedTokenRepo) GetByToken(ctx context.Context, token string) (*loka.WorkerToken, error) {
	return r.s.local.Tokens().GetByToken(ctx, token)
}
func (r *replicatedTokenRepo) MarkUsed(ctx context.Context, id, workerID string) error {
	return r.s.apply(ctx, "token", "mark_used", struct {
		ID       string `json:"id"`
		WorkerID string `json:"worker_id"`
	}{id, workerID})
}
func (r *replicatedTokenRepo) Delete(ctx context.Context, id string) error {
	return r.s.apply(ctx, "token", "delete", id)
}
func (r *replicatedTokenRepo) List(ctx context.Context) ([]*loka.WorkerToken, error) {
	return r.s.local.Tokens().List(ctx)
}
func (r *replicatedTokenRepo) DeleteExpiredBefore(ctx context.Context, before time.Time) (int, error) {
	if err := r.s.apply(ctx, "token", "delete_expired_before", before); err != nil {
		return 0, err
	}
	return 0, nil
}

var _ store.Store = (*Store)(nil)
