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

type workerRepo struct {
	db *sql.DB
}

func (r *workerRepo) Create(ctx context.Context, w *loka.Worker) error {
	labels, _ := json.Marshal(w.Labels)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO workers (id, hostname, ip_address, provider, region, zone, status, labels, capacity_cpu, capacity_mem, capacity_disk, agent_version, kvm_available, created_at, updated_at, last_seen)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		w.ID, w.Hostname, w.IPAddress, w.Provider, w.Region, w.Zone,
		string(w.Status), labels,
		w.Capacity.CPUCores, w.Capacity.MemoryMB, w.Capacity.DiskMB,
		w.AgentVersion, w.KVMAvailable, w.CreatedAt, w.UpdatedAt, w.LastSeen,
	)
	return err
}

func (r *workerRepo) Get(ctx context.Context, id string) (*loka.Worker, error) {
	var w loka.Worker
	var status string
	var labels []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT id, hostname, ip_address, provider, region, zone, status, labels, capacity_cpu, capacity_mem, capacity_disk, agent_version, kvm_available, created_at, updated_at, last_seen
		 FROM workers WHERE id = $1`, id).
		Scan(&w.ID, &w.Hostname, &w.IPAddress, &w.Provider, &w.Region, &w.Zone,
			&status, &labels,
			&w.Capacity.CPUCores, &w.Capacity.MemoryMB, &w.Capacity.DiskMB,
			&w.AgentVersion, &w.KVMAvailable, &w.CreatedAt, &w.UpdatedAt, &w.LastSeen)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("worker not found")
		}
		return nil, err
	}
	w.Status = loka.WorkerStatus(status)
	json.Unmarshal(labels, &w.Labels)
	return &w, nil
}

func (r *workerRepo) Update(ctx context.Context, w *loka.Worker) error {
	labels, _ := json.Marshal(w.Labels)
	w.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE workers SET hostname=$1, ip_address=$2, provider=$3, region=$4, zone=$5, status=$6, labels=$7, capacity_cpu=$8, capacity_mem=$9, capacity_disk=$10, agent_version=$11, kvm_available=$12, updated_at=$13, last_seen=$14
		 WHERE id=$15`,
		w.Hostname, w.IPAddress, w.Provider, w.Region, w.Zone,
		string(w.Status), labels,
		w.Capacity.CPUCores, w.Capacity.MemoryMB, w.Capacity.DiskMB,
		w.AgentVersion, w.KVMAvailable, w.UpdatedAt, w.LastSeen, w.ID,
	)
	return err
}

func (r *workerRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM workers WHERE id = $1`, id)
	return err
}

func (r *workerRepo) List(ctx context.Context, f store.WorkerFilter) ([]*loka.Worker, error) {
	query := `SELECT id, hostname, ip_address, provider, region, zone, status, labels, capacity_cpu, capacity_mem, capacity_disk, agent_version, kvm_available, created_at, updated_at, last_seen FROM workers WHERE TRUE`
	var args []any
	n := 0
	if f.Provider != nil {
		n++
		query += fmt.Sprintf(` AND provider = $%d`, n)
		args = append(args, *f.Provider)
	}
	if f.Status != nil {
		n++
		query += fmt.Sprintf(` AND status = $%d`, n)
		args = append(args, string(*f.Status))
	}
	if f.Region != nil {
		n++
		query += fmt.Sprintf(` AND region = $%d`, n)
		args = append(args, *f.Region)
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

	var workers []*loka.Worker
	for rows.Next() {
		var w loka.Worker
		var status string
		var labels []byte
		if err := rows.Scan(&w.ID, &w.Hostname, &w.IPAddress, &w.Provider, &w.Region, &w.Zone,
			&status, &labels,
			&w.Capacity.CPUCores, &w.Capacity.MemoryMB, &w.Capacity.DiskMB,
			&w.AgentVersion, &w.KVMAvailable, &w.CreatedAt, &w.UpdatedAt, &w.LastSeen); err != nil {
			return nil, err
		}
		w.Status = loka.WorkerStatus(status)
		json.Unmarshal(labels, &w.Labels)
		workers = append(workers, &w)
	}
	return workers, rows.Err()
}

func (r *workerRepo) UpdateHeartbeat(ctx context.Context, id string, hb *loka.Heartbeat) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE workers SET status=$1, last_seen=$2, updated_at=$3 WHERE id=$4`,
		string(hb.Status), hb.Timestamp, time.Now(), id,
	)
	return err
}

var _ store.WorkerRepository = (*workerRepo)(nil)
