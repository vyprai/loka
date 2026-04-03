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

type volumeRepo struct {
	db     *sql.DB // write pool
	readDB *sql.DB // read pool
}

var _ store.VolumeRepository = (*volumeRepo)(nil)

const volumeCols = `name, type, status, provider, size_bytes, max_size_bytes,
	primary_worker_id, replica_worker_ids, desired_replicas, mount_count,
	bucket, prefix, region, credentials, created_at, updated_at`

func (r *volumeRepo) Create(ctx context.Context, vol *loka.VolumeRecord) error {
	replicaJSON, err := json.Marshal(vol.ReplicaWorkerIDs)
	if err != nil {
		return fmt.Errorf("marshal replica_worker_ids: %w", err)
	}
	if vol.DesiredReplicas == 0 {
		vol.DesiredReplicas = 2
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO volumes (`+volumeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		vol.Name, string(vol.Type), string(vol.Status), vol.Provider,
		vol.SizeBytes, vol.MaxSizeBytes,
		vol.PrimaryWorkerID, string(replicaJSON), vol.DesiredReplicas, vol.MountCount,
		vol.Bucket, vol.Prefix, vol.Region, vol.Credentials,
		vol.CreatedAt.Format(time.RFC3339), vol.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert volume: %w", err)
	}
	return nil
}

func (r *volumeRepo) Get(ctx context.Context, name string) (*loka.VolumeRecord, error) {
	row := r.readDB.QueryRowContext(ctx,
		`SELECT `+volumeCols+` FROM volumes WHERE name = ?`, name)
	return scanVolume(row)
}

func (r *volumeRepo) Update(ctx context.Context, vol *loka.VolumeRecord) error {
	replicaJSON, err := json.Marshal(vol.ReplicaWorkerIDs)
	if err != nil {
		return fmt.Errorf("marshal replica_worker_ids: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE volumes SET type = ?, status = ?, provider = ?, size_bytes = ?, max_size_bytes = ?,
		primary_worker_id = ?, replica_worker_ids = ?, desired_replicas = ?, mount_count = ?,
		bucket = ?, prefix = ?, region = ?, credentials = ?, updated_at = ? WHERE name = ?`,
		string(vol.Type), string(vol.Status), vol.Provider,
		vol.SizeBytes, vol.MaxSizeBytes,
		vol.PrimaryWorkerID, string(replicaJSON), vol.DesiredReplicas, vol.MountCount,
		vol.Bucket, vol.Prefix, vol.Region, vol.Credentials,
		vol.UpdatedAt.Format(time.RFC3339), vol.Name,
	)
	return err
}

func (r *volumeRepo) Delete(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM volumes WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("volume %q not found", name)
	}
	return nil
}

func (r *volumeRepo) List(ctx context.Context) ([]*loka.VolumeRecord, error) {
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT `+volumeCols+` FROM volumes ORDER BY name LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vols []*loka.VolumeRecord
	for rows.Next() {
		vol, err := scanVolumeRow(rows)
		if err != nil {
			return nil, err
		}
		vols = append(vols, vol)
	}
	return vols, rows.Err()
}

func (r *volumeRepo) ListByWorker(ctx context.Context, workerID string) ([]*loka.VolumeRecord, error) {
	// Match primary OR replica (replica_worker_ids is a JSON array).
	likePattern := `%"` + workerID + `"%`
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT `+volumeCols+` FROM volumes
		WHERE primary_worker_id = ? OR replica_worker_ids LIKE ?
		ORDER BY name`, workerID, likePattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vols []*loka.VolumeRecord
	for rows.Next() {
		vol, err := scanVolumeRow(rows)
		if err != nil {
			return nil, err
		}
		vols = append(vols, vol)
	}
	return vols, rows.Err()
}

func (r *volumeRepo) UpdatePlacement(ctx context.Context, name, primaryWorkerID string, replicaWorkerIDs []string) error {
	replicaJSON, err := json.Marshal(replicaWorkerIDs)
	if err != nil {
		return fmt.Errorf("marshal replica_worker_ids: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE volumes SET primary_worker_id = ?, replica_worker_ids = ?, updated_at = ? WHERE name = ?`,
		primaryWorkerID, string(replicaJSON), time.Now().Format(time.RFC3339), name)
	return err
}

func (r *volumeRepo) UpdateStatus(ctx context.Context, name string, status loka.VolumeStatus) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET status = ?, updated_at = ? WHERE name = ?`,
		string(status), time.Now().Format(time.RFC3339), name)
	return err
}

func (r *volumeRepo) IncrementMountCount(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET mount_count = mount_count + 1, updated_at = ? WHERE name = ?`,
		time.Now().Format(time.RFC3339), name)
	return err
}

func (r *volumeRepo) DecrementMountCount(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET mount_count = MAX(mount_count - 1, 0), updated_at = ? WHERE name = ?`,
		time.Now().Format(time.RFC3339), name)
	return err
}

func scanVolume(row *sql.Row) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	var volType, status, replicaJSON, createdAt, updatedAt string
	err := row.Scan(
		&vol.Name, &volType, &status, &vol.Provider,
		&vol.SizeBytes, &vol.MaxSizeBytes,
		&vol.PrimaryWorkerID, &replicaJSON, &vol.DesiredReplicas, &vol.MountCount,
		&vol.Bucket, &vol.Prefix, &vol.Region, &vol.Credentials,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("volume not found")
		}
		return nil, err
	}
	vol.Type = loka.VolumeType(volType)
	vol.Status = loka.VolumeStatus(status)
	_ = json.Unmarshal([]byte(replicaJSON), &vol.ReplicaWorkerIDs)
	vol.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	vol.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &vol, nil
}

func scanVolumeRow(rows *sql.Rows) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	var volType, status, replicaJSON, createdAt, updatedAt string
	err := rows.Scan(
		&vol.Name, &volType, &status, &vol.Provider,
		&vol.SizeBytes, &vol.MaxSizeBytes,
		&vol.PrimaryWorkerID, &replicaJSON, &vol.DesiredReplicas, &vol.MountCount,
		&vol.Bucket, &vol.Prefix, &vol.Region, &vol.Credentials,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	vol.Type = loka.VolumeType(volType)
	vol.Status = loka.VolumeStatus(status)
	_ = json.Unmarshal([]byte(replicaJSON), &vol.ReplicaWorkerIDs)
	vol.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	vol.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &vol, nil
}
