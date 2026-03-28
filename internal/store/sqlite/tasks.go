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

type taskRepo struct {
	db     *sql.DB
	readDB *sql.DB
}

func (r *taskRepo) Create(ctx context.Context, t *loka.Task) error {
	args, _ := json.Marshal(t.Args)
	env, _ := json.Marshal(t.Env)
	mounts, _ := json.Marshal(t.Mounts)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO tasks (id, name, status, exit_code, worker_id, image_ref, command, args, env, workdir, bundle_key, vcpus, memory_mb, mounts, timeout, status_message, started_at, completed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, string(t.Status), t.ExitCode, t.WorkerID, t.ImageRef,
		t.Command, string(args), string(env), t.Workdir, t.BundleKey,
		t.VCPUs, t.MemoryMB, string(mounts), t.Timeout, t.StatusMessage,
		formatTime(t.StartedAt), formatTime(t.CompletedAt),
		t.CreatedAt.UTC().Format(time.RFC3339), t.UpdatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (r *taskRepo) Get(ctx context.Context, id string) (*loka.Task, error) {
	row := r.readDB.QueryRowContext(ctx,
		`SELECT id, name, status, exit_code, worker_id, image_ref, command, args, env, workdir, bundle_key, vcpus, memory_mb, mounts, timeout, status_message, started_at, completed_at, created_at, updated_at
		 FROM tasks WHERE id = ? OR name = ?`, id, id)
	return scanTask(row)
}

func (r *taskRepo) Update(ctx context.Context, t *loka.Task) error {
	args, _ := json.Marshal(t.Args)
	env, _ := json.Marshal(t.Env)
	mounts, _ := json.Marshal(t.Mounts)
	t.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET name=?, status=?, exit_code=?, worker_id=?, image_ref=?, command=?, args=?, env=?, workdir=?, bundle_key=?, vcpus=?, memory_mb=?, mounts=?, timeout=?, status_message=?, started_at=?, completed_at=?, updated_at=?
		 WHERE id=?`,
		t.Name, string(t.Status), t.ExitCode, t.WorkerID, t.ImageRef,
		t.Command, string(args), string(env), t.Workdir, t.BundleKey,
		t.VCPUs, t.MemoryMB, string(mounts), t.Timeout, t.StatusMessage,
		formatTime(t.StartedAt), formatTime(t.CompletedAt),
		t.UpdatedAt.UTC().Format(time.RFC3339), t.ID,
	)
	return err
}

func (r *taskRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ? OR name = ?`, id, id)
	return err
}

func (r *taskRepo) List(ctx context.Context, f store.TaskFilter) ([]*loka.Task, error) {
	query := `SELECT id, name, status, exit_code, worker_id, image_ref, command, args, env, workdir, bundle_key, vcpus, memory_mb, mounts, timeout, status_message, started_at, completed_at, created_at, updated_at FROM tasks WHERE 1=1`
	var qargs []any
	if f.Status != nil {
		query += ` AND status = ?`
		qargs = append(qargs, string(*f.Status))
	}
	if f.Name != nil {
		query += ` AND name = ?`
		qargs = append(qargs, *f.Name)
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	if f.Offset > 0 {
		query += fmt.Sprintf(` OFFSET %d`, f.Offset)
	}

	rows, err := r.readDB.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*loka.Task
	for rows.Next() {
		t, err := scanTaskRow(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func scanTask(row *sql.Row) (*loka.Task, error) {
	var t loka.Task
	var argsJSON, envJSON, mountsJSON, startedAt, completedAt, createdAt, updatedAt string
	err := row.Scan(
		&t.ID, &t.Name, &t.Status, &t.ExitCode, &t.WorkerID, &t.ImageRef,
		&t.Command, &argsJSON, &envJSON, &t.Workdir, &t.BundleKey,
		&t.VCPUs, &t.MemoryMB, &mountsJSON, &t.Timeout, &t.StatusMessage,
		&startedAt, &completedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(argsJSON), &t.Args)
	json.Unmarshal([]byte(envJSON), &t.Env)
	json.Unmarshal([]byte(mountsJSON), &t.Mounts)
	t.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	t.CompletedAt, _ = time.Parse(time.RFC3339, completedAt)
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	t.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &t, nil
}

func scanTaskRow(rows *sql.Rows) (*loka.Task, error) {
	var t loka.Task
	var argsJSON, envJSON, mountsJSON, startedAt, completedAt, createdAt, updatedAt string
	err := rows.Scan(
		&t.ID, &t.Name, &t.Status, &t.ExitCode, &t.WorkerID, &t.ImageRef,
		&t.Command, &argsJSON, &envJSON, &t.Workdir, &t.BundleKey,
		&t.VCPUs, &t.MemoryMB, &mountsJSON, &t.Timeout, &t.StatusMessage,
		&startedAt, &completedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(argsJSON), &t.Args)
	json.Unmarshal([]byte(envJSON), &t.Env)
	json.Unmarshal([]byte(mountsJSON), &t.Mounts)
	t.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	t.CompletedAt, _ = time.Parse(time.RFC3339, completedAt)
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	t.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &t, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
