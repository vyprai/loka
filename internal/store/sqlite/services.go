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

type serviceRepo struct {
	db *sql.DB
}

func (r *serviceRepo) Create(ctx context.Context, svc *loka.Service) error {
	args, _ := json.Marshal(svc.Args)
	env, _ := json.Marshal(svc.Env)
	routes, _ := json.Marshal(svc.Routes)
	labels, _ := json.Marshal(svc.Labels)
	mounts, _ := json.Marshal(svc.Mounts)
	autoscale, _ := json.Marshal(svc.Autoscale)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO services (id, name, status, worker_id, image_ref, image_id, recipe_name, command, args, env, workdir, port, vcpus, memory_mb, routes, bundle_key, idle_timeout, health_path, health_interval, health_timeout, health_retries, labels, mounts, autoscale, snapshot_id, ready, status_message, last_activity, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		svc.ID, svc.Name, string(svc.Status), svc.WorkerID,
		svc.ImageRef, svc.ImageID, svc.RecipeName, svc.Command,
		string(args), string(env), svc.Workdir, svc.Port,
		svc.VCPUs, svc.MemoryMB, string(routes), svc.BundleKey,
		svc.IdleTimeout, svc.HealthPath, svc.HealthInterval, svc.HealthTimeout, svc.HealthRetries,
		string(labels), string(mounts), string(autoscale),
		svc.SnapshotID, boolToInt(svc.Ready), svc.StatusMessage,
		svc.LastActivity.UTC().Format(time.RFC3339),
		svc.CreatedAt.UTC().Format(time.RFC3339),
		svc.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

func (r *serviceRepo) Get(ctx context.Context, id string) (*loka.Service, error) {
	row := r.db.QueryRowContext(ctx, serviceSelectSQL+` WHERE id = ?`, id)
	return scanService(row)
}

func (r *serviceRepo) Update(ctx context.Context, svc *loka.Service) error {
	args, _ := json.Marshal(svc.Args)
	env, _ := json.Marshal(svc.Env)
	routes, _ := json.Marshal(svc.Routes)
	labels, _ := json.Marshal(svc.Labels)
	mounts, _ := json.Marshal(svc.Mounts)
	autoscale, _ := json.Marshal(svc.Autoscale)
	svc.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE services SET name=?, status=?, worker_id=?, image_ref=?, image_id=?, recipe_name=?, command=?, args=?, env=?, workdir=?, port=?, vcpus=?, memory_mb=?, routes=?, bundle_key=?, idle_timeout=?, health_path=?, health_interval=?, health_timeout=?, health_retries=?, labels=?, mounts=?, autoscale=?, snapshot_id=?, ready=?, status_message=?, last_activity=?, updated_at=?
		 WHERE id=?`,
		svc.Name, string(svc.Status), svc.WorkerID,
		svc.ImageRef, svc.ImageID, svc.RecipeName, svc.Command,
		string(args), string(env), svc.Workdir, svc.Port,
		svc.VCPUs, svc.MemoryMB, string(routes), svc.BundleKey,
		svc.IdleTimeout, svc.HealthPath, svc.HealthInterval, svc.HealthTimeout, svc.HealthRetries,
		string(labels), string(mounts), string(autoscale),
		svc.SnapshotID, boolToInt(svc.Ready), svc.StatusMessage,
		svc.LastActivity.UTC().Format(time.RFC3339),
		svc.UpdatedAt.UTC().Format(time.RFC3339),
		svc.ID,
	)
	if err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	return nil
}

func (r *serviceRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id)
	return err
}

func (r *serviceRepo) List(ctx context.Context, f store.ServiceFilter) ([]*loka.Service, int, error) {
	where := ` WHERE 1=1`
	var args []any
	if f.Status != nil {
		where += ` AND status = ?`
		args = append(args, string(*f.Status))
	}
	if f.WorkerID != nil {
		where += ` AND worker_id = ?`
		args = append(args, *f.WorkerID)
	}
	if f.Name != nil {
		where += ` AND name = ?`
		args = append(args, *f.Name)
	}

	// Count total matching rows.
	var total int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM services`+where, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count services: %w", err)
	}

	query := serviceSelectSQL + where + ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	if f.Offset > 0 {
		query += fmt.Sprintf(` OFFSET %d`, f.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	var services []*loka.Service
	for rows.Next() {
		svc, err := scanServiceRows(rows)
		if err != nil {
			return nil, 0, err
		}
		services = append(services, svc)
	}
	return services, total, rows.Err()
}

func (r *serviceRepo) ListByWorker(ctx context.Context, workerID string) ([]*loka.Service, error) {
	svcs, _, err := r.List(ctx, store.ServiceFilter{WorkerID: &workerID})
	return svcs, err
}

const serviceSelectSQL = `SELECT id, name, status, worker_id, image_ref, image_id, recipe_name, command, args, env, workdir, port, vcpus, memory_mb, routes, bundle_key, idle_timeout, health_path, health_interval, health_timeout, health_retries, labels, mounts, autoscale, snapshot_id, ready, status_message, last_activity, created_at, updated_at FROM services`

func scanService(row *sql.Row) (*loka.Service, error) {
	var svc loka.Service
	var status, argsJSON, envJSON, routesJSON, labelsJSON, mountsJSON, autoscaleJSON string
	var lastActivity, createdAt, updatedAt string
	var ready int
	err := row.Scan(
		&svc.ID, &svc.Name, &status, &svc.WorkerID,
		&svc.ImageRef, &svc.ImageID, &svc.RecipeName, &svc.Command,
		&argsJSON, &envJSON, &svc.Workdir, &svc.Port,
		&svc.VCPUs, &svc.MemoryMB, &routesJSON, &svc.BundleKey,
		&svc.IdleTimeout, &svc.HealthPath, &svc.HealthInterval, &svc.HealthTimeout, &svc.HealthRetries,
		&labelsJSON, &mountsJSON, &autoscaleJSON,
		&svc.SnapshotID, &ready, &svc.StatusMessage,
		&lastActivity, &createdAt, &updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("service not found")
		}
		return nil, fmt.Errorf("scan service: %w", err)
	}
	svc.Status = loka.ServiceStatus(status)
	svc.Ready = ready != 0
	json.Unmarshal([]byte(argsJSON), &svc.Args)
	json.Unmarshal([]byte(envJSON), &svc.Env)
	json.Unmarshal([]byte(routesJSON), &svc.Routes)
	json.Unmarshal([]byte(labelsJSON), &svc.Labels)
	json.Unmarshal([]byte(mountsJSON), &svc.Mounts)
	json.Unmarshal([]byte(autoscaleJSON), &svc.Autoscale)
	svc.LastActivity, _ = time.Parse(time.RFC3339, lastActivity)
	svc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	svc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &svc, nil
}

func scanServiceRows(rows *sql.Rows) (*loka.Service, error) {
	var svc loka.Service
	var status, argsJSON, envJSON, routesJSON, labelsJSON, mountsJSON, autoscaleJSON string
	var lastActivity, createdAt, updatedAt string
	var ready int
	err := rows.Scan(
		&svc.ID, &svc.Name, &status, &svc.WorkerID,
		&svc.ImageRef, &svc.ImageID, &svc.RecipeName, &svc.Command,
		&argsJSON, &envJSON, &svc.Workdir, &svc.Port,
		&svc.VCPUs, &svc.MemoryMB, &routesJSON, &svc.BundleKey,
		&svc.IdleTimeout, &svc.HealthPath, &svc.HealthInterval, &svc.HealthTimeout, &svc.HealthRetries,
		&labelsJSON, &mountsJSON, &autoscaleJSON,
		&svc.SnapshotID, &ready, &svc.StatusMessage,
		&lastActivity, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan service row: %w", err)
	}
	svc.Status = loka.ServiceStatus(status)
	svc.Ready = ready != 0
	json.Unmarshal([]byte(argsJSON), &svc.Args)
	json.Unmarshal([]byte(envJSON), &svc.Env)
	json.Unmarshal([]byte(routesJSON), &svc.Routes)
	json.Unmarshal([]byte(labelsJSON), &svc.Labels)
	json.Unmarshal([]byte(mountsJSON), &svc.Mounts)
	json.Unmarshal([]byte(autoscaleJSON), &svc.Autoscale)
	svc.LastActivity, _ = time.Parse(time.RFC3339, lastActivity)
	svc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	svc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &svc, nil
}

var _ store.ServiceRepository = (*serviceRepo)(nil)
