package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type tokenRepo struct {
	db *sql.DB
}

func (r *tokenRepo) Create(ctx context.Context, t *loka.WorkerToken) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO worker_tokens (id, name, token, expires_at, used, worker_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Token,
		t.ExpiresAt.UTC().Format(time.RFC3339),
		boolToInt(t.Used), t.WorkerID,
		t.CreatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (r *tokenRepo) Get(ctx context.Context, id string) (*loka.WorkerToken, error) {
	var t loka.WorkerToken
	var expiresAt, createdAt string
	var used int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, token, expires_at, used, worker_id, created_at FROM worker_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Token, &expiresAt, &used, &t.WorkerID, &createdAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("token not found")
		}
		return nil, err
	}
	t.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	t.Used = used != 0
	return &t, nil
}

func (r *tokenRepo) GetByToken(ctx context.Context, token string) (*loka.WorkerToken, error) {
	var t loka.WorkerToken
	var expiresAt, createdAt string
	var used int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, token, expires_at, used, worker_id, created_at FROM worker_tokens WHERE token = ?`, token).
		Scan(&t.ID, &t.Name, &t.Token, &expiresAt, &used, &t.WorkerID, &createdAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("token not found")
		}
		return nil, err
	}
	t.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	t.Used = used != 0
	return &t, nil
}

func (r *tokenRepo) MarkUsed(ctx context.Context, id, workerID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE worker_tokens SET used = 1, worker_id = ? WHERE id = ?`, workerID, id)
	return err
}

func (r *tokenRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM worker_tokens WHERE id = ?`, id)
	return err
}

func (r *tokenRepo) List(ctx context.Context) ([]*loka.WorkerToken, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, token, expires_at, used, worker_id, created_at FROM worker_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []*loka.WorkerToken
	for rows.Next() {
		var t loka.WorkerToken
		var expiresAt, createdAt string
		var used int
		if err := rows.Scan(&t.ID, &t.Name, &t.Token, &expiresAt, &used, &t.WorkerID, &createdAt); err != nil {
			return nil, err
		}
		t.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		t.Used = used != 0
		tokens = append(tokens, &t)
	}
	return tokens, rows.Err()
}

func (r *tokenRepo) DeleteExpiredBefore(ctx context.Context, before time.Time) (int, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM worker_tokens WHERE expires_at != '' AND expires_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("delete expired tokens: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

var _ store.TokenRepository = (*tokenRepo)(nil)
