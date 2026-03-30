package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type serviceRepo struct {
	db     *sql.DB // write pool
	readDB *sql.DB // read pool
}

func marshalDatabaseConfig(cfg *loka.DatabaseConfig) string {
	if cfg == nil {
		return ""
	}
	// Encrypt password before persisting.
	cp := *cfg
	cp.Password = loka.EncryptPassword(cp.Password)
	data, _ := json.Marshal(&cp)
	return string(data)
}

func unmarshalDatabaseConfig(s string, svc *loka.Service) {
	if s == "" {
		return
	}
	var cfg loka.DatabaseConfig
	if json.Unmarshal([]byte(s), &cfg) == nil {
		// Decrypt password after loading.
		cfg.Password = loka.DecryptPassword(cfg.Password)
		svc.DatabaseConfig = &cfg
	}
}

func (r *serviceRepo) Create(ctx context.Context, svc *loka.Service) error {
	args, err := json.Marshal(svc.Args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	env, err := json.Marshal(svc.Env)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	routes, err := json.Marshal(svc.Routes)
	if err != nil {
		return fmt.Errorf("marshal routes: %w", err)
	}
	labels, err := json.Marshal(svc.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	mounts, err := json.Marshal(svc.Mounts)
	if err != nil {
		return fmt.Errorf("marshal mounts: %w", err)
	}
	autoscale, err := json.Marshal(svc.Autoscale)
	if err != nil {
		return fmt.Errorf("marshal autoscale: %w", err)
	}
	usesJSON, err := json.Marshal(svc.Uses)
	if err != nil {
		return fmt.Errorf("marshal uses: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO services (id, name, status, worker_id, image_ref, image_id, recipe_name, command, args, env, workdir, port, vcpus, memory_mb, routes, bundle_key, idle_timeout, health_path, health_interval, health_timeout, health_retries, labels, mounts, autoscale, snapshot_id, app_snapshot_mem, app_snapshot_state, forward_port, ready, status_message, database_config, uses, parent_service_id, replicas, relation_type, last_activity, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		svc.ID, svc.Name, string(svc.Status), svc.WorkerID,
		svc.ImageRef, svc.ImageID, svc.RecipeName, svc.Command,
		string(args), string(env), svc.Workdir, svc.Port,
		svc.VCPUs, svc.MemoryMB, string(routes), svc.BundleKey,
		svc.IdleTimeout, svc.HealthPath, svc.HealthInterval, svc.HealthTimeout, svc.HealthRetries,
		string(labels), string(mounts), string(autoscale),
		svc.SnapshotID, svc.AppSnapshotMem, svc.AppSnapshotState,
		svc.ForwardPort, boolToInt(svc.Ready), svc.StatusMessage,
		marshalDatabaseConfig(svc.DatabaseConfig),
		string(usesJSON),
		svc.ParentServiceID, svc.Replicas, svc.RelationType,
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
	row := r.readDB.QueryRowContext(ctx, serviceSelectSQL+` WHERE id = ?`, id)
	return scanService(row)
}

func (r *serviceRepo) Update(ctx context.Context, svc *loka.Service) error {
	args, err := json.Marshal(svc.Args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	env, err := json.Marshal(svc.Env)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	routes, err := json.Marshal(svc.Routes)
	if err != nil {
		return fmt.Errorf("marshal routes: %w", err)
	}
	labels, err := json.Marshal(svc.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	mounts, err := json.Marshal(svc.Mounts)
	if err != nil {
		return fmt.Errorf("marshal mounts: %w", err)
	}
	autoscale, err := json.Marshal(svc.Autoscale)
	if err != nil {
		return fmt.Errorf("marshal autoscale: %w", err)
	}
	usesJSON, err := json.Marshal(svc.Uses)
	if err != nil {
		return fmt.Errorf("marshal uses: %w", err)
	}
	svc.UpdatedAt = time.Now()
	_, err = r.db.ExecContext(ctx,
		`UPDATE services SET name=?, status=?, worker_id=?, image_ref=?, image_id=?, recipe_name=?, command=?, args=?, env=?, workdir=?, port=?, vcpus=?, memory_mb=?, routes=?, bundle_key=?, idle_timeout=?, health_path=?, health_interval=?, health_timeout=?, health_retries=?, labels=?, mounts=?, autoscale=?, snapshot_id=?, app_snapshot_mem=?, app_snapshot_state=?, forward_port=?, ready=?, status_message=?, database_config=?, uses=?, parent_service_id=?, replicas=?, relation_type=?, last_activity=?, updated_at=?
		 WHERE id=?`,
		svc.Name, string(svc.Status), svc.WorkerID,
		svc.ImageRef, svc.ImageID, svc.RecipeName, svc.Command,
		string(args), string(env), svc.Workdir, svc.Port,
		svc.VCPUs, svc.MemoryMB, string(routes), svc.BundleKey,
		svc.IdleTimeout, svc.HealthPath, svc.HealthInterval, svc.HealthTimeout, svc.HealthRetries,
		string(labels), string(mounts), string(autoscale),
		svc.SnapshotID, svc.AppSnapshotMem, svc.AppSnapshotState,
		svc.ForwardPort, boolToInt(svc.Ready), svc.StatusMessage,
		marshalDatabaseConfig(svc.DatabaseConfig),
		string(usesJSON),
		svc.ParentServiceID, svc.Replicas, svc.RelationType,
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
	if f.IsDatabase != nil {
		if *f.IsDatabase {
			where += ` AND database_config != ''`
		} else {
			where += ` AND database_config = ''`
		}
	}
	if f.ParentServiceID != nil {
		where += ` AND parent_service_id = ?`
		args = append(args, *f.ParentServiceID)
	}
	if f.PrimaryID != nil {
		// Escape LIKE wildcards in the PrimaryID to prevent injection.
		escaped := strings.ReplaceAll(*f.PrimaryID, `%`, `\%`)
		escaped = strings.ReplaceAll(escaped, `_`, `\_`)
		where += ` AND database_config LIKE ? ESCAPE '\'`
		args = append(args, `%"primary_id":"`+escaped+`"%`)
	}

	// Count total matching rows.
	var total int
	err := r.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM services`+where, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count services: %w", err)
	}

	query := serviceSelectSQL + where + ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	if f.Offset > 0 {
		query += ` OFFSET ?`
		args = append(args, f.Offset)
	}

	rows, err := r.readDB.QueryContext(ctx, query, args...)
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

const serviceSelectSQL = `SELECT id, name, status, worker_id, image_ref, image_id, recipe_name, command, args, env, workdir, port, vcpus, memory_mb, routes, bundle_key, idle_timeout, health_path, health_interval, health_timeout, health_retries, labels, mounts, autoscale, snapshot_id, app_snapshot_mem, app_snapshot_state, forward_port, ready, status_message, database_config, uses, parent_service_id, replicas, relation_type, last_activity, created_at, updated_at FROM services`

func scanService(row *sql.Row) (*loka.Service, error) {
	var svc loka.Service
	var status, argsJSON, envJSON, routesJSON, labelsJSON, mountsJSON, autoscaleJSON string
	var databaseConfigJSON, usesJSON string
	var lastActivity, createdAt, updatedAt string
	var ready int
	err := row.Scan(
		&svc.ID, &svc.Name, &status, &svc.WorkerID,
		&svc.ImageRef, &svc.ImageID, &svc.RecipeName, &svc.Command,
		&argsJSON, &envJSON, &svc.Workdir, &svc.Port,
		&svc.VCPUs, &svc.MemoryMB, &routesJSON, &svc.BundleKey,
		&svc.IdleTimeout, &svc.HealthPath, &svc.HealthInterval, &svc.HealthTimeout, &svc.HealthRetries,
		&labelsJSON, &mountsJSON, &autoscaleJSON,
		&svc.SnapshotID, &svc.AppSnapshotMem, &svc.AppSnapshotState,
		&svc.ForwardPort, &ready, &svc.StatusMessage,
		&databaseConfigJSON, &usesJSON,
		&svc.ParentServiceID, &svc.Replicas, &svc.RelationType,
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
	unmarshalDatabaseConfig(databaseConfigJSON, &svc)
	json.Unmarshal([]byte(usesJSON), &svc.Uses)
	if err := json.Unmarshal([]byte(argsJSON), &svc.Args); err != nil {
		return nil, fmt.Errorf("unmarshal args: %w", err)
	}
	if err := json.Unmarshal([]byte(envJSON), &svc.Env); err != nil {
		return nil, fmt.Errorf("unmarshal env: %w", err)
	}
	if err := json.Unmarshal([]byte(routesJSON), &svc.Routes); err != nil {
		return nil, fmt.Errorf("unmarshal routes: %w", err)
	}
	if err := json.Unmarshal([]byte(labelsJSON), &svc.Labels); err != nil {
		return nil, fmt.Errorf("unmarshal labels: %w", err)
	}
	if err := json.Unmarshal([]byte(mountsJSON), &svc.Mounts); err != nil {
		return nil, fmt.Errorf("unmarshal mounts: %w", err)
	}
	if err := json.Unmarshal([]byte(autoscaleJSON), &svc.Autoscale); err != nil {
		return nil, fmt.Errorf("unmarshal autoscale: %w", err)
	}
	svc.LastActivity, _ = time.Parse(time.RFC3339, lastActivity)
	svc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	svc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &svc, nil
}

func scanServiceRows(rows *sql.Rows) (*loka.Service, error) {
	var svc loka.Service
	var status, argsJSON, envJSON, routesJSON, labelsJSON, mountsJSON, autoscaleJSON string
	var databaseConfigJSON, usesJSON string
	var lastActivity, createdAt, updatedAt string
	var ready int
	err := rows.Scan(
		&svc.ID, &svc.Name, &status, &svc.WorkerID,
		&svc.ImageRef, &svc.ImageID, &svc.RecipeName, &svc.Command,
		&argsJSON, &envJSON, &svc.Workdir, &svc.Port,
		&svc.VCPUs, &svc.MemoryMB, &routesJSON, &svc.BundleKey,
		&svc.IdleTimeout, &svc.HealthPath, &svc.HealthInterval, &svc.HealthTimeout, &svc.HealthRetries,
		&labelsJSON, &mountsJSON, &autoscaleJSON,
		&svc.SnapshotID, &svc.AppSnapshotMem, &svc.AppSnapshotState,
		&svc.ForwardPort, &ready, &svc.StatusMessage,
		&databaseConfigJSON, &usesJSON,
		&svc.ParentServiceID, &svc.Replicas, &svc.RelationType,
		&lastActivity, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan service row: %w", err)
	}
	svc.Status = loka.ServiceStatus(status)
	svc.Ready = ready != 0
	unmarshalDatabaseConfig(databaseConfigJSON, &svc)
	json.Unmarshal([]byte(usesJSON), &svc.Uses)
	if err := json.Unmarshal([]byte(argsJSON), &svc.Args); err != nil {
		return nil, fmt.Errorf("unmarshal args: %w", err)
	}
	if err := json.Unmarshal([]byte(envJSON), &svc.Env); err != nil {
		return nil, fmt.Errorf("unmarshal env: %w", err)
	}
	if err := json.Unmarshal([]byte(routesJSON), &svc.Routes); err != nil {
		return nil, fmt.Errorf("unmarshal routes: %w", err)
	}
	if err := json.Unmarshal([]byte(labelsJSON), &svc.Labels); err != nil {
		return nil, fmt.Errorf("unmarshal labels: %w", err)
	}
	if err := json.Unmarshal([]byte(mountsJSON), &svc.Mounts); err != nil {
		return nil, fmt.Errorf("unmarshal mounts: %w", err)
	}
	if err := json.Unmarshal([]byte(autoscaleJSON), &svc.Autoscale); err != nil {
		return nil, fmt.Errorf("unmarshal autoscale: %w", err)
	}
	svc.LastActivity, _ = time.Parse(time.RFC3339, lastActivity)
	svc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	svc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &svc, nil
}

// ListIdleCandidates returns services that might need idling, selecting only
// the fields needed for the idle check (avoids deserializing all JSON columns).
func (r *serviceRepo) ListIdleCandidates(ctx context.Context) ([]store.IdleCandidate, error) {
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT id, name, idle_timeout, last_activity FROM services WHERE status = 'running' AND idle_timeout > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []store.IdleCandidate
	for rows.Next() {
		var c store.IdleCandidate
		var lastActivity string
		if err := rows.Scan(&c.ID, &c.Name, &c.IdleTimeout, &lastActivity); err != nil {
			return nil, err
		}
		c.LastActivity, _ = time.Parse(time.RFC3339, lastActivity)
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

var _ store.ServiceRepository = (*serviceRepo)(nil)
