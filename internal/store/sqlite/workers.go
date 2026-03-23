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

type workerRepo struct {
	db *sql.DB
}

func (r *workerRepo) Create(ctx context.Context, w *loka.Worker) error {
	labels, _ := json.Marshal(w.Labels)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO workers (id, hostname, ip_address, provider, region, zone, status, labels, capacity_cpu, capacity_mem, capacity_disk, agent_version, kvm_available, created_at, updated_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Hostname, w.IPAddress, w.Provider, w.Region, w.Zone,
		string(w.Status), string(labels),
		w.Capacity.CPUCores, w.Capacity.MemoryMB, w.Capacity.DiskMB,
		w.AgentVersion, boolToInt(w.KVMAvailable),
		w.CreatedAt.UTC().Format(time.RFC3339),
		w.UpdatedAt.UTC().Format(time.RFC3339),
		w.LastSeen.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("create worker: %w", err)
	}
	return nil
}

func (r *workerRepo) Get(ctx context.Context, id string) (*loka.Worker, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, hostname, ip_address, provider, region, zone, status, labels, capacity_cpu, capacity_mem, capacity_disk, agent_version, kvm_available, created_at, updated_at, last_seen
		 FROM workers WHERE id = ?`, id)
	return scanWorker(row)
}

func (r *workerRepo) Update(ctx context.Context, w *loka.Worker) error {
	labels, _ := json.Marshal(w.Labels)
	w.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE workers SET hostname=?, ip_address=?, provider=?, region=?, zone=?, status=?, labels=?, capacity_cpu=?, capacity_mem=?, capacity_disk=?, agent_version=?, kvm_available=?, updated_at=?, last_seen=?
		 WHERE id=?`,
		w.Hostname, w.IPAddress, w.Provider, w.Region, w.Zone,
		string(w.Status), string(labels),
		w.Capacity.CPUCores, w.Capacity.MemoryMB, w.Capacity.DiskMB,
		w.AgentVersion, boolToInt(w.KVMAvailable),
		w.UpdatedAt.UTC().Format(time.RFC3339),
		w.LastSeen.UTC().Format(time.RFC3339), w.ID,
	)
	if err != nil {
		return fmt.Errorf("update worker: %w", err)
	}
	return nil
}

func (r *workerRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM workers WHERE id = ?`, id)
	return err
}

func (r *workerRepo) List(ctx context.Context, f store.WorkerFilter) ([]*loka.Worker, error) {
	query := `SELECT id, hostname, ip_address, provider, region, zone, status, labels, capacity_cpu, capacity_mem, capacity_disk, agent_version, kvm_available, created_at, updated_at, last_seen FROM workers WHERE 1=1`
	var args []any
	if f.Provider != nil {
		query += ` AND provider = ?`
		args = append(args, *f.Provider)
	}
	if f.Status != nil {
		query += ` AND status = ?`
		args = append(args, string(*f.Status))
	}
	if f.Region != nil {
		query += ` AND region = ?`
		args = append(args, *f.Region)
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
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()

	var workers []*loka.Worker
	for rows.Next() {
		w, err := scanWorkerRows(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

func (r *workerRepo) UpdateHeartbeat(ctx context.Context, id string, hb *loka.Heartbeat) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE workers SET status=?, last_seen=?, updated_at=? WHERE id=?`,
		string(hb.Status), hb.Timestamp.UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339), id,
	)
	return err
}

func scanWorker(row *sql.Row) (*loka.Worker, error) {
	var w loka.Worker
	var status, labels, createdAt, updatedAt, lastSeen string
	var kvm int
	err := row.Scan(&w.ID, &w.Hostname, &w.IPAddress, &w.Provider, &w.Region, &w.Zone,
		&status, &labels,
		&w.Capacity.CPUCores, &w.Capacity.MemoryMB, &w.Capacity.DiskMB,
		&w.AgentVersion, &kvm, &createdAt, &updatedAt, &lastSeen)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("worker not found")
		}
		return nil, fmt.Errorf("scan worker: %w", err)
	}
	w.Status = loka.WorkerStatus(status)
	w.KVMAvailable = kvm != 0
	json.Unmarshal([]byte(labels), &w.Labels)
	w.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	w.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	w.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
	return &w, nil
}

func scanWorkerRows(rows *sql.Rows) (*loka.Worker, error) {
	var w loka.Worker
	var status, labels, createdAt, updatedAt, lastSeen string
	var kvm int
	err := rows.Scan(&w.ID, &w.Hostname, &w.IPAddress, &w.Provider, &w.Region, &w.Zone,
		&status, &labels,
		&w.Capacity.CPUCores, &w.Capacity.MemoryMB, &w.Capacity.DiskMB,
		&w.AgentVersion, &kvm, &createdAt, &updatedAt, &lastSeen)
	if err != nil {
		return nil, fmt.Errorf("scan worker row: %w", err)
	}
	w.Status = loka.WorkerStatus(status)
	w.KVMAvailable = kvm != 0
	json.Unmarshal([]byte(labels), &w.Labels)
	w.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	w.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	w.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
	return &w, nil
}

var _ store.WorkerRepository = (*workerRepo)(nil)
