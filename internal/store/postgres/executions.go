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

type executionRepo struct {
	db *sql.DB
}

func (r *executionRepo) Create(ctx context.Context, e *loka.Execution) error {
	cmds, _ := json.Marshal(e.Commands)
	results, _ := json.Marshal(e.Results)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO executions (id, session_id, status, parallel, commands, results, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ID, e.SessionID, string(e.Status), e.Parallel, cmds, results, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (r *executionRepo) Get(ctx context.Context, id string) (*loka.Execution, error) {
	var e loka.Execution
	var status string
	var cmds, results []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT id, session_id, status, parallel, commands, results, created_at, updated_at
		 FROM executions WHERE id = $1`, id).
		Scan(&e.ID, &e.SessionID, &status, &e.Parallel, &cmds, &results, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("execution not found")
		}
		return nil, err
	}
	e.Status = loka.ExecStatus(status)
	json.Unmarshal(cmds, &e.Commands)
	json.Unmarshal(results, &e.Results)
	return &e, nil
}

func (r *executionRepo) Update(ctx context.Context, e *loka.Execution) error {
	cmds, _ := json.Marshal(e.Commands)
	results, _ := json.Marshal(e.Results)
	e.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE executions SET status=$1, parallel=$2, commands=$3, results=$4, updated_at=$5 WHERE id=$6`,
		string(e.Status), e.Parallel, cmds, results, e.UpdatedAt, e.ID,
	)
	return err
}

func (r *executionRepo) ListBySession(ctx context.Context, sessionID string, f store.ExecutionFilter) ([]*loka.Execution, error) {
	query := `SELECT id, session_id, status, parallel, commands, results, created_at, updated_at FROM executions WHERE session_id = $1`
	args := []any{sessionID}
	n := 1
	if f.Status != nil {
		n++
		query += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, string(*f.Status))
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		n++
		query += fmt.Sprintf(` LIMIT $%d`, n)
		args = append(args, f.Limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var execs []*loka.Execution
	for rows.Next() {
		var e loka.Execution
		var status string
		var cmds, results []byte
		if err := rows.Scan(&e.ID, &e.SessionID, &status, &e.Parallel, &cmds, &results, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Status = loka.ExecStatus(status)
		json.Unmarshal(cmds, &e.Commands)
		json.Unmarshal(results, &e.Results)
		execs = append(execs, &e)
	}
	return execs, rows.Err()
}

var _ store.ExecutionRepository = (*executionRepo)(nil)
