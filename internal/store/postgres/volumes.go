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

type volumeRepo struct {
	db *sql.DB
}

var _ store.VolumeRepository = (*volumeRepo)(nil)

const pgVolumeCols = `name, type, status, provider, size_bytes, max_size_bytes,
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
		`INSERT INTO volumes (`+pgVolumeCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		vol.Name, string(vol.Type), string(vol.Status), vol.Provider,
		vol.SizeBytes, vol.MaxSizeBytes,
		vol.PrimaryWorkerID, string(replicaJSON), vol.DesiredReplicas, vol.MountCount,
		vol.Bucket, vol.Prefix, vol.Region, vol.Credentials,
		vol.CreatedAt, vol.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert volume: %w", err)
	}
	return nil
}

func (r *volumeRepo) Get(ctx context.Context, name string) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	var volType, status, replicaJSON string
	err := r.db.QueryRowContext(ctx,
		`SELECT `+pgVolumeCols+` FROM volumes WHERE name = $1`, name).
		Scan(&vol.Name, &volType, &status, &vol.Provider,
			&vol.SizeBytes, &vol.MaxSizeBytes,
			&vol.PrimaryWorkerID, &replicaJSON, &vol.DesiredReplicas, &vol.MountCount,
			&vol.Bucket, &vol.Prefix, &vol.Region, &vol.Credentials,
			&vol.CreatedAt, &vol.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("volume not found")
		}
		return nil, fmt.Errorf("get volume: %w", err)
	}
	vol.Type = loka.VolumeType(volType)
	vol.Status = loka.VolumeStatus(status)
	_ = json.Unmarshal([]byte(replicaJSON), &vol.ReplicaWorkerIDs)
	return &vol, nil
}

func (r *volumeRepo) Update(ctx context.Context, vol *loka.VolumeRecord) error {
	vol.UpdatedAt = time.Now()
	replicaJSON, err := json.Marshal(vol.ReplicaWorkerIDs)
	if err != nil {
		return fmt.Errorf("marshal replica_worker_ids: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE volumes SET type=$1, status=$2, provider=$3, size_bytes=$4, max_size_bytes=$5,
		primary_worker_id=$6, replica_worker_ids=$7, desired_replicas=$8, mount_count=$9,
		bucket=$10, prefix=$11, region=$12, credentials=$13, updated_at=$14 WHERE name=$15`,
		string(vol.Type), string(vol.Status), vol.Provider,
		vol.SizeBytes, vol.MaxSizeBytes,
		vol.PrimaryWorkerID, string(replicaJSON), vol.DesiredReplicas, vol.MountCount,
		vol.Bucket, vol.Prefix, vol.Region, vol.Credentials,
		vol.UpdatedAt, vol.Name,
	)
	return err
}

func (r *volumeRepo) Delete(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM volumes WHERE name = $1`, name)
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
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+pgVolumeCols+` FROM volumes ORDER BY name LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vols []*loka.VolumeRecord
	for rows.Next() {
		vol, err := scanPgVolumeRow(rows)
		if err != nil {
			return nil, err
		}
		vols = append(vols, vol)
	}
	return vols, rows.Err()
}

func (r *volumeRepo) ListByWorker(ctx context.Context, workerID string) ([]*loka.VolumeRecord, error) {
	likePattern := `%"` + workerID + `"%`
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+pgVolumeCols+` FROM volumes
		WHERE primary_worker_id = $1 OR replica_worker_ids::text LIKE $2
		ORDER BY name`, workerID, likePattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vols []*loka.VolumeRecord
	for rows.Next() {
		vol, err := scanPgVolumeRow(rows)
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
		`UPDATE volumes SET primary_worker_id = $1, replica_worker_ids = $2, updated_at = $3 WHERE name = $4`,
		primaryWorkerID, string(replicaJSON), time.Now(), name)
	return err
}

func (r *volumeRepo) UpdateStatus(ctx context.Context, name string, status loka.VolumeStatus) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET status = $1, updated_at = $2 WHERE name = $3`,
		string(status), time.Now(), name)
	return err
}

func (r *volumeRepo) IncrementMountCount(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET mount_count = mount_count + 1, updated_at = $1 WHERE name = $2`,
		time.Now(), name)
	return err
}

func (r *volumeRepo) DecrementMountCount(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET mount_count = GREATEST(mount_count - 1, 0), updated_at = $1 WHERE name = $2`,
		time.Now(), name)
	return err
}

func scanPgVolumeRow(rows *sql.Rows) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	var volType, status, replicaJSON string
	err := rows.Scan(
		&vol.Name, &volType, &status, &vol.Provider,
		&vol.SizeBytes, &vol.MaxSizeBytes,
		&vol.PrimaryWorkerID, &replicaJSON, &vol.DesiredReplicas, &vol.MountCount,
		&vol.Bucket, &vol.Prefix, &vol.Region, &vol.Credentials,
		&vol.CreatedAt, &vol.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	vol.Type = loka.VolumeType(volType)
	vol.Status = loka.VolumeStatus(status)
	_ = json.Unmarshal([]byte(replicaJSON), &vol.ReplicaWorkerIDs)
	return &vol, nil
}
