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

type executionRepo struct {
	db *sql.DB
}

func (r *executionRepo) Create(ctx context.Context, e *loka.Execution) error {
	cmds, _ := json.Marshal(e.Commands)
	results, _ := json.Marshal(e.Results)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO executions (id, session_id, status, parallel, commands, results, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.SessionID, string(e.Status), boolToInt(e.Parallel),
		string(cmds), string(results),
		e.CreatedAt.UTC().Format(time.RFC3339), e.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	return nil
}

func (r *executionRepo) Get(ctx context.Context, id string) (*loka.Execution, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, session_id, status, parallel, commands, results, created_at, updated_at
		 FROM executions WHERE id = ?`, id)
	return scanExecution(row)
}

func (r *executionRepo) Update(ctx context.Context, e *loka.Execution) error {
	cmds, _ := json.Marshal(e.Commands)
	results, _ := json.Marshal(e.Results)
	e.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE executions SET status=?, parallel=?, commands=?, results=?, updated_at=? WHERE id=?`,
		string(e.Status), boolToInt(e.Parallel), string(cmds), string(results),
		e.UpdatedAt.UTC().Format(time.RFC3339), e.ID,
	)
	if err != nil {
		return fmt.Errorf("update execution: %w", err)
	}
	return nil
}

func (r *executionRepo) ListBySession(ctx context.Context, sessionID string, f store.ExecutionFilter) ([]*loka.Execution, error) {
	query := `SELECT id, session_id, status, parallel, commands, results, created_at, updated_at FROM executions WHERE session_id = ?`
	args := []any{sessionID}
	if f.Status != nil {
		query += ` AND status = ?`
		args = append(args, string(*f.Status))
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
		return nil, fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()

	var execs []*loka.Execution
	for rows.Next() {
		e, err := scanExecutionRows(rows)
		if err != nil {
			return nil, err
		}
		execs = append(execs, e)
	}
	return execs, rows.Err()
}

func scanExecution(row *sql.Row) (*loka.Execution, error) {
	var e loka.Execution
	var status, cmds, results, createdAt, updatedAt string
	var parallel int
	err := row.Scan(&e.ID, &e.SessionID, &status, &parallel, &cmds, &results, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("execution not found")
		}
		return nil, fmt.Errorf("scan execution: %w", err)
	}
	e.Status = loka.ExecStatus(status)
	e.Parallel = parallel != 0
	json.Unmarshal([]byte(cmds), &e.Commands)
	json.Unmarshal([]byte(results), &e.Results)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &e, nil
}

func scanExecutionRows(rows *sql.Rows) (*loka.Execution, error) {
	var e loka.Execution
	var status, cmds, results, createdAt, updatedAt string
	var parallel int
	err := rows.Scan(&e.ID, &e.SessionID, &status, &parallel, &cmds, &results, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("scan execution row: %w", err)
	}
	e.Status = loka.ExecStatus(status)
	e.Parallel = parallel != 0
	json.Unmarshal([]byte(cmds), &e.Commands)
	json.Unmarshal([]byte(results), &e.Results)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &e, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (r *executionRepo) DeleteCompletedBefore(ctx context.Context, before time.Time) (int, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM executions WHERE status IN ('success', 'failed', 'canceled') AND updated_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("delete completed executions: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

var _ store.ExecutionRepository = (*executionRepo)(nil)
