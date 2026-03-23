package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type sessionRepo struct {
	db *sql.DB
}

func (r *sessionRepo) Create(ctx context.Context, s *loka.Session) error {
	labels, _ := json.Marshal(s.Labels)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions (id, name, status, mode, worker_id, image_ref, vcpus, memory_mb, labels, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		s.ID, s.Name, string(s.Status), string(s.Mode), s.WorkerID, s.ImageRef,
		s.VCPUs, s.MemoryMB, labels, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *sessionRepo) Get(ctx context.Context, id string) (*loka.Session, error) {
	var s loka.Session
	var status, mode string
	var labels []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, status, mode, worker_id, image_ref, vcpus, memory_mb, labels, created_at, updated_at
		 FROM sessions WHERE id = $1`, id).
		Scan(&s.ID, &s.Name, &status, &mode, &s.WorkerID, &s.ImageRef,
			&s.VCPUs, &s.MemoryMB, &labels, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	s.Status = loka.SessionStatus(status)
	s.Mode = loka.ExecMode(mode)
	json.Unmarshal(labels, &s.Labels)
	return &s, nil
}

func (r *sessionRepo) Update(ctx context.Context, s *loka.Session) error {
	labels, _ := json.Marshal(s.Labels)
	s.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET name=$1, status=$2, mode=$3, worker_id=$4, image_ref=$5, vcpus=$6, memory_mb=$7, labels=$8, updated_at=$9
		 WHERE id=$10`,
		s.Name, string(s.Status), string(s.Mode), s.WorkerID, s.ImageRef,
		s.VCPUs, s.MemoryMB, labels, s.UpdatedAt, s.ID,
	)
	return err
}

func (r *sessionRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

func (r *sessionRepo) List(ctx context.Context, f store.SessionFilter) ([]*loka.Session, error) {
	query := `SELECT id, name, status, mode, worker_id, image_ref, vcpus, memory_mb, labels, created_at, updated_at FROM sessions WHERE TRUE`
	var args []any
	n := 0
	if f.Status != nil {
		n++
		query += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, string(*f.Status))
	}
	if f.WorkerID != nil {
		n++
		query += fmt.Sprintf(` AND worker_id = $%d`, n)
		args = append(args, *f.WorkerID)
	}
	if f.Name != nil {
		n++
		query += fmt.Sprintf(` AND name = $%d`, n)
		args = append(args, *f.Name)
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		n++
		query += fmt.Sprintf(` LIMIT $%d`, n)
		args = append(args, f.Limit)
	}
	if f.Offset > 0 {
		n++
		query += fmt.Sprintf(` OFFSET $%d`, n)
		args = append(args, f.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*loka.Session
	for rows.Next() {
		var s loka.Session
		var status, mode string
		var labels []byte
		if err := rows.Scan(&s.ID, &s.Name, &status, &mode, &s.WorkerID, &s.ImageRef,
			&s.VCPUs, &s.MemoryMB, &labels, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Status = loka.SessionStatus(status)
		s.Mode = loka.ExecMode(mode)
		json.Unmarshal(labels, &s.Labels)
		sessions = append(sessions, &s)
	}
	return sessions, rows.Err()
}

func (r *sessionRepo) ListByWorker(ctx context.Context, workerID string) ([]*loka.Session, error) {
	return r.List(ctx, store.SessionFilter{WorkerID: &workerID})
}

func (r *sessionRepo) DeleteTerminatedBefore(ctx context.Context, before time.Time) (int, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE status = 'terminated' AND updated_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("delete terminated sessions: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

var _ store.SessionRepository = (*sessionRepo)(nil)
