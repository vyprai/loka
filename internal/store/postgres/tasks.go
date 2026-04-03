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

type taskRepo struct {
	db *sql.DB
}

var _ store.TaskRepository = (*taskRepo)(nil)

func (r *taskRepo) Create(ctx context.Context, t *loka.Task) error {
	args, err := json.Marshal(t.Args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	env, err := json.Marshal(t.Env)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	mounts, err := json.Marshal(t.Mounts)
	if err != nil {
		return fmt.Errorf("marshal mounts: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO tasks (id, name, status, exit_code, worker_id, image_ref, command, args, env, workdir, bundle_key, vcpus, memory_mb, mounts, timeout, status_message, started_at, completed_at, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		t.ID, t.Name, string(t.Status), t.ExitCode, t.WorkerID, t.ImageRef,
		t.Command, string(args), string(env), t.Workdir, t.BundleKey,
		t.VCPUs, t.MemoryMB, string(mounts), t.Timeout, t.StatusMessage,
		t.StartedAt, t.CompletedAt, t.CreatedAt, t.UpdatedAt,
	)
	return err
}

func (r *taskRepo) Get(ctx context.Context, id string) (*loka.Task, error) {
	var t loka.Task
	var argsJSON, envJSON, mountsJSON string
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, status, exit_code, worker_id, image_ref, command, args, env, workdir, bundle_key, vcpus, memory_mb, mounts, timeout, status_message, started_at, completed_at, created_at, updated_at
		 FROM tasks WHERE id = $1 OR name = $1`, id).
		Scan(
			&t.ID, &t.Name, &t.Status, &t.ExitCode, &t.WorkerID, &t.ImageRef,
			&t.Command, &argsJSON, &envJSON, &t.Workdir, &t.BundleKey,
			&t.VCPUs, &t.MemoryMB, &mountsJSON, &t.Timeout, &t.StatusMessage,
			&t.StartedAt, &t.CompletedAt, &t.CreatedAt, &t.UpdatedAt,
		)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("task %q not found", id)
		}
		return nil, fmt.Errorf("get task: %w", err)
	}
	json.Unmarshal([]byte(argsJSON), &t.Args)
	json.Unmarshal([]byte(envJSON), &t.Env)
	json.Unmarshal([]byte(mountsJSON), &t.Mounts)
	return &t, nil
}

func (r *taskRepo) Update(ctx context.Context, t *loka.Task) error {
	args, err := json.Marshal(t.Args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	env, err := json.Marshal(t.Env)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	mounts, err := json.Marshal(t.Mounts)
	if err != nil {
		return fmt.Errorf("marshal mounts: %w", err)
	}
	t.UpdatedAt = time.Now()
	_, err = r.db.ExecContext(ctx,
		`UPDATE tasks SET name=$1, status=$2, exit_code=$3, worker_id=$4, image_ref=$5, command=$6, args=$7, env=$8, workdir=$9, bundle_key=$10, vcpus=$11, memory_mb=$12, mounts=$13, timeout=$14, status_message=$15, started_at=$16, completed_at=$17, updated_at=$18
		 WHERE id=$19`,
		t.Name, string(t.Status), t.ExitCode, t.WorkerID, t.ImageRef,
		t.Command, string(args), string(env), t.Workdir, t.BundleKey,
		t.VCPUs, t.MemoryMB, string(mounts), t.Timeout, t.StatusMessage,
		t.StartedAt, t.CompletedAt, t.UpdatedAt, t.ID,
	)
	return err
}

func (r *taskRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1 OR name = $1`, id)
	return err
}

func (r *taskRepo) List(ctx context.Context, f store.TaskFilter) ([]*loka.Task, error) {
	query := `SELECT id, name, status, exit_code, worker_id, image_ref, command, args, env, workdir, bundle_key, vcpus, memory_mb, mounts, timeout, status_message, started_at, completed_at, created_at, updated_at FROM tasks WHERE 1=1`
	var qargs []any
	n := 1
	if f.Status != nil {
		query += fmt.Sprintf(` AND status = $%d`, n)
		qargs = append(qargs, string(*f.Status))
		n++
	}
	if f.Name != nil {
		query += fmt.Sprintf(` AND name = $%d`, n)
		qargs = append(qargs, *f.Name)
		n++
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	if f.Offset > 0 {
		query += fmt.Sprintf(` OFFSET %d`, f.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*loka.Task
	for rows.Next() {
		var t loka.Task
		var argsJSON, envJSON, mountsJSON string
		if err := rows.Scan(
			&t.ID, &t.Name, &t.Status, &t.ExitCode, &t.WorkerID, &t.ImageRef,
			&t.Command, &argsJSON, &envJSON, &t.Workdir, &t.BundleKey,
			&t.VCPUs, &t.MemoryMB, &mountsJSON, &t.Timeout, &t.StatusMessage,
			&t.StartedAt, &t.CompletedAt, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(argsJSON), &t.Args)
		json.Unmarshal([]byte(envJSON), &t.Env)
		json.Unmarshal([]byte(mountsJSON), &t.Mounts)
		tasks = append(tasks, &t)
	}
	return tasks, rows.Err()
}
