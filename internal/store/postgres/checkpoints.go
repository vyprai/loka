package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type checkpointRepo struct {
	db *sql.DB
}

func (r *checkpointRepo) Create(ctx context.Context, cp *loka.Checkpoint) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO checkpoints (id, session_id, parent_id, type, status, label, overlay_path, vmstate_path, metadata_path, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (id) DO UPDATE SET status=$5, label=$6, overlay_path=$7, vmstate_path=$8, metadata_path=$9`,
		cp.ID, cp.SessionID, cp.ParentID, string(cp.Type), string(cp.Status),
		cp.Label, cp.OverlayPath, cp.VMStatePath, cp.MetadataPath, cp.CreatedAt,
	)
	return err
}

func (r *checkpointRepo) Get(ctx context.Context, id string) (*loka.Checkpoint, error) {
	var cp loka.Checkpoint
	var cpType, status string
	err := r.db.QueryRowContext(ctx,
		`SELECT id, session_id, parent_id, type, status, label, overlay_path, vmstate_path, metadata_path, created_at
		 FROM checkpoints WHERE id = $1`, id).
		Scan(&cp.ID, &cp.SessionID, &cp.ParentID, &cpType, &status,
			&cp.Label, &cp.OverlayPath, &cp.VMStatePath, &cp.MetadataPath, &cp.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("checkpoint not found")
		}
		return nil, err
	}
	cp.Type = loka.CheckpointType(cpType)
	cp.Status = loka.CheckpointStatus(status)
	return &cp, nil
}

func (r *checkpointRepo) GetDAG(ctx context.Context, sessionID string) (*loka.CheckpointDAG, error) {
	cps, err := r.ListBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	dag := loka.NewCheckpointDAG(sessionID)
	for _, cp := range cps {
		dag.Add(cp)
	}
	return dag, nil
}

func (r *checkpointRepo) ListBySession(ctx context.Context, sessionID string) ([]*loka.Checkpoint, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, parent_id, type, status, label, overlay_path, vmstate_path, metadata_path, created_at
		 FROM checkpoints WHERE session_id = $1 ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cps []*loka.Checkpoint
	for rows.Next() {
		var cp loka.Checkpoint
		var cpType, status string
		if err := rows.Scan(&cp.ID, &cp.SessionID, &cp.ParentID, &cpType, &status,
			&cp.Label, &cp.OverlayPath, &cp.VMStatePath, &cp.MetadataPath, &cp.CreatedAt); err != nil {
			return nil, err
		}
		cp.Type = loka.CheckpointType(cpType)
		cp.Status = loka.CheckpointStatus(status)
		cps = append(cps, &cp)
	}
	return cps, rows.Err()
}

func (r *checkpointRepo) Update(ctx context.Context, cp *loka.Checkpoint) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE checkpoints SET session_id=$1, parent_id=$2, type=$3, status=$4, label=$5, overlay_path=$6, vmstate_path=$7, metadata_path=$8
		 WHERE id=$9`,
		cp.SessionID, cp.ParentID, string(cp.Type), string(cp.Status),
		cp.Label, cp.OverlayPath, cp.VMStatePath, cp.MetadataPath, cp.ID,
	)
	return err
}

func (r *checkpointRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE id = $1`, id)
	return err
}

func (r *checkpointRepo) DeleteSubtree(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		WITH RECURSIVE descendants(id) AS (
			SELECT id FROM checkpoints WHERE id = $1
			UNION ALL
			SELECT c.id FROM checkpoints c JOIN descendants d ON c.parent_id = d.id
		)
		DELETE FROM checkpoints WHERE id IN (SELECT id FROM descendants)
	`, id)
	return err
}

var _ store.CheckpointRepository = (*checkpointRepo)(nil)
