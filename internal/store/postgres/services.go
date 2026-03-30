package postgres

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
	db *sql.DB
}

func (r *serviceRepo) Create(ctx context.Context, svc *loka.Service) error {
	args, _ := json.Marshal(svc.Args)
	env, _ := json.Marshal(svc.Env)
	routes, _ := json.Marshal(svc.Routes)
	labels, _ := json.Marshal(svc.Labels)
	mounts, _ := json.Marshal(svc.Mounts)
	autoscale, _ := json.Marshal(svc.Autoscale)
	dbConfig := marshalDatabaseConfig(svc.DatabaseConfig)
	usesJSON, _ := json.Marshal(svc.Uses)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO services (id, name, status, worker_id, image_ref, image_id, recipe_name, command, args, env, workdir, port, vcpus, memory_mb, routes, bundle_key, idle_timeout, health_path, health_interval, health_timeout, health_retries, labels, mounts, autoscale, snapshot_id, app_snapshot_mem, app_snapshot_state, forward_port, ready, status_message, database_config, uses, parent_service_id, replicas, relation_type, last_activity, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38)`,
		svc.ID, svc.Name, string(svc.Status), svc.WorkerID,
		svc.ImageRef, svc.ImageID, svc.RecipeName, svc.Command,
		string(args), string(env), svc.Workdir, svc.Port,
		svc.VCPUs, svc.MemoryMB, string(routes), svc.BundleKey,
		svc.IdleTimeout, svc.HealthPath, svc.HealthInterval, svc.HealthTimeout, svc.HealthRetries,
		string(labels), string(mounts), string(autoscale),
		svc.SnapshotID, svc.AppSnapshotMem, svc.AppSnapshotState,
		svc.ForwardPort, svc.Ready, svc.StatusMessage,
		dbConfig, string(usesJSON),
		svc.ParentServiceID, svc.Replicas, svc.RelationType,
		svc.LastActivity, svc.CreatedAt, svc.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

func (r *serviceRepo) Get(ctx context.Context, id string) (*loka.Service, error) {
	row := r.db.QueryRowContext(ctx, serviceSelectSQL+` WHERE id = $1`, id)
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
	dbConfig := marshalDatabaseConfig(svc.DatabaseConfig)
	usesJSON, _ := json.Marshal(svc.Uses)
	_, err := r.db.ExecContext(ctx,
		`UPDATE services SET name=$1, status=$2, worker_id=$3, image_ref=$4, image_id=$5, recipe_name=$6, command=$7, args=$8, env=$9, workdir=$10, port=$11, vcpus=$12, memory_mb=$13, routes=$14, bundle_key=$15, idle_timeout=$16, health_path=$17, health_interval=$18, health_timeout=$19, health_retries=$20, labels=$21, mounts=$22, autoscale=$23, snapshot_id=$24, app_snapshot_mem=$25, app_snapshot_state=$26, forward_port=$27, ready=$28, status_message=$29, database_config=$30, uses=$31, parent_service_id=$32, replicas=$33, relation_type=$34, last_activity=$35, updated_at=$36
		 WHERE id=$37`,
		svc.Name, string(svc.Status), svc.WorkerID,
		svc.ImageRef, svc.ImageID, svc.RecipeName, svc.Command,
		string(args), string(env), svc.Workdir, svc.Port,
		svc.VCPUs, svc.MemoryMB, string(routes), svc.BundleKey,
		svc.IdleTimeout, svc.HealthPath, svc.HealthInterval, svc.HealthTimeout, svc.HealthRetries,
		string(labels), string(mounts), string(autoscale),
		svc.SnapshotID, svc.AppSnapshotMem, svc.AppSnapshotState,
		svc.ForwardPort, svc.Ready, svc.StatusMessage,
		dbConfig, string(usesJSON),
		svc.ParentServiceID, svc.Replicas, svc.RelationType,
		svc.LastActivity, svc.UpdatedAt,
		svc.ID,
	)
	if err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	return nil
}

func (r *serviceRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM services WHERE id = $1`, id)
	return err
}

func (r *serviceRepo) List(ctx context.Context, f store.ServiceFilter) ([]*loka.Service, int, error) {
	where := ` WHERE 1=1`
	var args []any
	n := 1
	if f.Status != nil {
		where += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, string(*f.Status))
		n++
	}
	if f.WorkerID != nil {
		where += fmt.Sprintf(` AND worker_id = $%d`, n)
		args = append(args, *f.WorkerID)
		n++
	}
	if f.Name != nil {
		where += fmt.Sprintf(` AND name = $%d`, n)
		args = append(args, *f.Name)
		n++
	}
	if f.IsDatabase != nil {
		if *f.IsDatabase {
			where += ` AND database_config != ''`
		} else {
			where += ` AND database_config = ''`
		}
	}
	if f.PrimaryID != nil {
		// Escape LIKE wildcards in the PrimaryID to prevent injection.
		escaped := strings.ReplaceAll(*f.PrimaryID, `%`, `\%`)
		escaped = strings.ReplaceAll(escaped, `_`, `\_`)
		where += fmt.Sprintf(` AND database_config LIKE $%d ESCAPE '\'`, n)
		args = append(args, `%"primary_id":"`+escaped+`"%`)
		n++
	}

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

const serviceSelectSQL = `SELECT id, name, status, worker_id, image_ref, image_id, recipe_name, command, args, env, workdir, port, vcpus, memory_mb, routes, bundle_key, idle_timeout, health_path, health_interval, health_timeout, health_retries, labels, mounts, autoscale, snapshot_id, app_snapshot_mem, app_snapshot_state, forward_port, ready, status_message, database_config, uses, parent_service_id, replicas, relation_type, last_activity, created_at, updated_at FROM services`

func marshalDatabaseConfig(cfg *loka.DatabaseConfig) string {
	if cfg == nil {
		return ""
	}
	data, _ := json.Marshal(cfg)
	return string(data)
}

func unmarshalDatabaseConfig(s string, svc *loka.Service) {
	if s == "" {
		return
	}
	var cfg loka.DatabaseConfig
	if json.Unmarshal([]byte(s), &cfg) == nil {
		svc.DatabaseConfig = &cfg
	}
}

func scanService(row *sql.Row) (*loka.Service, error) {
	var svc loka.Service
	var status, argsJSON, envJSON, routesJSON, labelsJSON, mountsJSON, autoscaleJSON string
	var databaseConfigJSON, usesJSON string
	var lastActivity, createdAt, updatedAt time.Time
	err := row.Scan(
		&svc.ID, &svc.Name, &status, &svc.WorkerID,
		&svc.ImageRef, &svc.ImageID, &svc.RecipeName, &svc.Command,
		&argsJSON, &envJSON, &svc.Workdir, &svc.Port,
		&svc.VCPUs, &svc.MemoryMB, &routesJSON, &svc.BundleKey,
		&svc.IdleTimeout, &svc.HealthPath, &svc.HealthInterval, &svc.HealthTimeout, &svc.HealthRetries,
		&labelsJSON, &mountsJSON, &autoscaleJSON,
		&svc.SnapshotID, &svc.AppSnapshotMem, &svc.AppSnapshotState,
		&svc.ForwardPort, &svc.Ready, &svc.StatusMessage,
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
	svc.LastActivity = lastActivity
	svc.CreatedAt = createdAt
	svc.UpdatedAt = updatedAt
	unmarshalDatabaseConfig(databaseConfigJSON, &svc)
	json.Unmarshal([]byte(usesJSON), &svc.Uses)
	json.Unmarshal([]byte(argsJSON), &svc.Args)
	json.Unmarshal([]byte(envJSON), &svc.Env)
	json.Unmarshal([]byte(routesJSON), &svc.Routes)
	json.Unmarshal([]byte(labelsJSON), &svc.Labels)
	json.Unmarshal([]byte(mountsJSON), &svc.Mounts)
	json.Unmarshal([]byte(autoscaleJSON), &svc.Autoscale)
	return &svc, nil
}

func scanServiceRows(rows *sql.Rows) (*loka.Service, error) {
	var svc loka.Service
	var status, argsJSON, envJSON, routesJSON, labelsJSON, mountsJSON, autoscaleJSON string
	var databaseConfigJSON, usesJSON string
	var lastActivity, createdAt, updatedAt time.Time
	err := rows.Scan(
		&svc.ID, &svc.Name, &status, &svc.WorkerID,
		&svc.ImageRef, &svc.ImageID, &svc.RecipeName, &svc.Command,
		&argsJSON, &envJSON, &svc.Workdir, &svc.Port,
		&svc.VCPUs, &svc.MemoryMB, &routesJSON, &svc.BundleKey,
		&svc.IdleTimeout, &svc.HealthPath, &svc.HealthInterval, &svc.HealthTimeout, &svc.HealthRetries,
		&labelsJSON, &mountsJSON, &autoscaleJSON,
		&svc.SnapshotID, &svc.AppSnapshotMem, &svc.AppSnapshotState,
		&svc.ForwardPort, &svc.Ready, &svc.StatusMessage,
		&databaseConfigJSON, &usesJSON,
		&svc.ParentServiceID, &svc.Replicas, &svc.RelationType,
		&lastActivity, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan service row: %w", err)
	}
	svc.Status = loka.ServiceStatus(status)
	svc.LastActivity = lastActivity
	svc.CreatedAt = createdAt
	svc.UpdatedAt = updatedAt
	unmarshalDatabaseConfig(databaseConfigJSON, &svc)
	json.Unmarshal([]byte(usesJSON), &svc.Uses)
	json.Unmarshal([]byte(argsJSON), &svc.Args)
	json.Unmarshal([]byte(envJSON), &svc.Env)
	json.Unmarshal([]byte(routesJSON), &svc.Routes)
	json.Unmarshal([]byte(labelsJSON), &svc.Labels)
	json.Unmarshal([]byte(mountsJSON), &svc.Mounts)
	json.Unmarshal([]byte(autoscaleJSON), &svc.Autoscale)
	return &svc, nil
}

func (r *serviceRepo) ListIdleCandidates(ctx context.Context) ([]store.IdleCandidate, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, idle_timeout, last_activity FROM services WHERE status = 'running' AND idle_timeout > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []store.IdleCandidate
	for rows.Next() {
		var c store.IdleCandidate
		if err := rows.Scan(&c.ID, &c.Name, &c.IdleTimeout, &c.LastActivity); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

var _ store.ServiceRepository = (*serviceRepo)(nil)
