package sqlite

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
	policy, _ := json.Marshal(s.ExecPolicy)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions (id, name, status, mode, worker_id, image_ref, image_id, snapshot_id, vcpus, memory_mb, labels, exec_policy, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, string(s.Status), string(s.Mode), s.WorkerID,
		s.ImageRef, s.ImageID, s.SnapshotID,
		s.VCPUs, s.MemoryMB, string(labels), string(policy),
		s.CreatedAt.UTC().Format(time.RFC3339), s.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *sessionRepo) Get(ctx context.Context, id string) (*loka.Session, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, name, status, mode, worker_id, image_ref, image_id, snapshot_id, vcpus, memory_mb, labels, exec_policy, created_at, updated_at
		 FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

func (r *sessionRepo) Update(ctx context.Context, s *loka.Session) error {
	labels, _ := json.Marshal(s.Labels)
	policy, _ := json.Marshal(s.ExecPolicy)
	s.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET name=?, status=?, mode=?, worker_id=?, image_ref=?, image_id=?, snapshot_id=?, vcpus=?, memory_mb=?, labels=?, exec_policy=?, updated_at=?
		 WHERE id=?`,
		s.Name, string(s.Status), string(s.Mode), s.WorkerID,
		s.ImageRef, s.ImageID, s.SnapshotID,
		s.VCPUs, s.MemoryMB, string(labels), string(policy),
		s.UpdatedAt.UTC().Format(time.RFC3339), s.ID,
	)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	return nil
}

func (r *sessionRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func (r *sessionRepo) List(ctx context.Context, f store.SessionFilter) ([]*loka.Session, error) {
	query := `SELECT id, name, status, mode, worker_id, image_ref, image_id, snapshot_id, vcpus, memory_mb, labels, exec_policy, created_at, updated_at FROM sessions WHERE 1=1`
	var args []any
	if f.Status != nil {
		query += ` AND status = ?`
		args = append(args, string(*f.Status))
	}
	if f.WorkerID != nil {
		query += ` AND worker_id = ?`
		args = append(args, *f.WorkerID)
	}
	if f.Name != nil {
		query += ` AND name = ?`
		args = append(args, *f.Name)
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	if f.Offset > 0 {
		query += fmt.Sprintf(` OFFSET %d`, f.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*loka.Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (r *sessionRepo) ListByWorker(ctx context.Context, workerID string) ([]*loka.Session, error) {
	status := loka.SessionStatus("")
	return r.List(ctx, store.SessionFilter{WorkerID: &workerID, Status: &status})
}

func scanSession(row *sql.Row) (*loka.Session, error) {
	var s loka.Session
	var labels, policy, status, mode, createdAt, updatedAt string
	err := row.Scan(&s.ID, &s.Name, &status, &mode, &s.WorkerID,
		&s.ImageRef, &s.ImageID, &s.SnapshotID,
		&s.VCPUs, &s.MemoryMB, &labels, &policy, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("scan session: %w", err)
	}
	s.Status = loka.SessionStatus(status)
	s.Mode = loka.ExecMode(mode)
	json.Unmarshal([]byte(labels), &s.Labels)
	json.Unmarshal([]byte(policy), &s.ExecPolicy)
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	s.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &s, nil
}

func scanSessionRows(rows *sql.Rows) (*loka.Session, error) {
	var s loka.Session
	var labels, policy, status, mode, createdAt, updatedAt string
	err := rows.Scan(&s.ID, &s.Name, &status, &mode, &s.WorkerID,
		&s.ImageRef, &s.ImageID, &s.SnapshotID,
		&s.VCPUs, &s.MemoryMB, &labels, &policy, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("scan session row: %w", err)
	}
	s.Status = loka.SessionStatus(status)
	s.Mode = loka.ExecMode(mode)
	json.Unmarshal([]byte(labels), &s.Labels)
	json.Unmarshal([]byte(policy), &s.ExecPolicy)
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	s.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &s, nil
}

func (r *sessionRepo) DeleteTerminatedBefore(ctx context.Context, before time.Time) (int, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE status = 'terminated' AND updated_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("delete terminated sessions: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

var _ store.SessionRepository = (*sessionRepo)(nil)
