package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type volumeRepo struct {
	db *sql.DB
}

var _ store.VolumeRepository = (*volumeRepo)(nil)

func (r *volumeRepo) Create(ctx context.Context, vol *loka.VolumeRecord) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO volumes (name, provider, mount_count, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		vol.Name, vol.Provider, vol.MountCount, vol.CreatedAt, vol.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert volume: %w", err)
	}
	return nil
}

func (r *volumeRepo) Get(ctx context.Context, name string) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	err := r.db.QueryRowContext(ctx,
		`SELECT name, provider, mount_count, created_at, updated_at FROM volumes WHERE name = $1`, name).
		Scan(&vol.Name, &vol.Provider, &vol.MountCount, &vol.CreatedAt, &vol.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("volume not found")
		}
		return nil, fmt.Errorf("get volume: %w", err)
	}
	return &vol, nil
}

func (r *volumeRepo) Update(ctx context.Context, vol *loka.VolumeRecord) error {
	vol.UpdatedAt = time.Now()
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET provider = $1, mount_count = $2, updated_at = $3 WHERE name = $4`,
		vol.Provider, vol.MountCount, vol.UpdatedAt, vol.Name,
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
		`SELECT name, provider, mount_count, created_at, updated_at FROM volumes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vols []*loka.VolumeRecord
	for rows.Next() {
		var vol loka.VolumeRecord
		if err := rows.Scan(&vol.Name, &vol.Provider, &vol.MountCount, &vol.CreatedAt, &vol.UpdatedAt); err != nil {
			return nil, err
		}
		vols = append(vols, &vol)
	}
	return vols, rows.Err()
}
