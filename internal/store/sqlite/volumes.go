package sqlite

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
		`INSERT INTO volumes (name, provider, mount_count, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		vol.Name, vol.Provider, vol.MountCount,
		vol.CreatedAt.Format(time.RFC3339), vol.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert volume: %w", err)
	}
	return nil
}

func (r *volumeRepo) Get(ctx context.Context, name string) (*loka.VolumeRecord, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT name, provider, mount_count, created_at, updated_at FROM volumes WHERE name = ?`, name)
	return scanVolume(row)
}

func (r *volumeRepo) Update(ctx context.Context, vol *loka.VolumeRecord) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE volumes SET provider = ?, mount_count = ?, updated_at = ? WHERE name = ?`,
		vol.Provider, vol.MountCount, vol.UpdatedAt.Format(time.RFC3339), vol.Name,
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
	rows, err := r.db.QueryContext(ctx,
		`SELECT name, provider, mount_count, created_at, updated_at FROM volumes ORDER BY name`)
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

func scanVolume(row *sql.Row) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	var createdAt, updatedAt string
	err := row.Scan(&vol.Name, &vol.Provider, &vol.MountCount, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("volume not found")
		}
		return nil, err
	}
	vol.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	vol.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &vol, nil
}

func scanVolumeRow(rows *sql.Rows) (*loka.VolumeRecord, error) {
	var vol loka.VolumeRecord
	var createdAt, updatedAt string
	err := rows.Scan(&vol.Name, &vol.Provider, &vol.MountCount, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	vol.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	vol.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &vol, nil
}
