package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type checkpointRepo struct {
	db *sql.DB
}

func (r *checkpointRepo) Create(ctx context.Context, cp *loka.Checkpoint) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO checkpoints (id, session_id, parent_id, type, status, label, overlay_path, vmstate_path, metadata_path, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.ID, cp.SessionID, cp.ParentID, string(cp.Type), string(cp.Status),
		cp.Label, cp.OverlayPath, cp.VMStatePath, cp.MetadataPath,
		cp.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("create checkpoint: %w", err)
	}
	return nil
}

func (r *checkpointRepo) Get(ctx context.Context, id string) (*loka.Checkpoint, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, session_id, parent_id, type, status, label, overlay_path, vmstate_path, metadata_path, created_at
		 FROM checkpoints WHERE id = ?`, id)
	return scanCheckpoint(row)
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
		 FROM checkpoints WHERE session_id = ? ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()

	var cps []*loka.Checkpoint
	for rows.Next() {
		cp, err := scanCheckpointRows(rows)
		if err != nil {
			return nil, err
		}
		cps = append(cps, cp)
	}
	return cps, rows.Err()
}

func (r *checkpointRepo) Update(ctx context.Context, cp *loka.Checkpoint) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE checkpoints SET session_id = ?, parent_id = ?, type = ?, status = ?, label = ?, overlay_path = ?, vmstate_path = ?, metadata_path = ?
		 WHERE id = ?`,
		cp.SessionID, cp.ParentID, string(cp.Type), string(cp.Status),
		cp.Label, cp.OverlayPath, cp.VMStatePath, cp.MetadataPath,
		cp.ID,
	)
	if err != nil {
		return fmt.Errorf("update checkpoint: %w", err)
	}
	return nil
}

func (r *checkpointRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE id = ?`, id)
	return err
}

func (r *checkpointRepo) DeleteSubtree(ctx context.Context, id string) error {
	// Recursively delete checkpoint and all descendants.
	// Use a CTE to find all descendants.
	_, err := r.db.ExecContext(ctx, `
		WITH RECURSIVE descendants(id) AS (
			SELECT id FROM checkpoints WHERE id = ?
			UNION ALL
			SELECT c.id FROM checkpoints c JOIN descendants d ON c.parent_id = d.id
		)
		DELETE FROM checkpoints WHERE id IN (SELECT id FROM descendants)
	`, id)
	return err
}

func scanCheckpoint(row *sql.Row) (*loka.Checkpoint, error) {
	var cp loka.Checkpoint
	var cpType, status, createdAt string
	err := row.Scan(&cp.ID, &cp.SessionID, &cp.ParentID, &cpType, &status,
		&cp.Label, &cp.OverlayPath, &cp.VMStatePath, &cp.MetadataPath, &createdAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("checkpoint not found")
		}
		return nil, fmt.Errorf("scan checkpoint: %w", err)
	}
	cp.Type = loka.CheckpointType(cpType)
	cp.Status = loka.CheckpointStatus(status)
	cp.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &cp, nil
}

func scanCheckpointRows(rows *sql.Rows) (*loka.Checkpoint, error) {
	var cp loka.Checkpoint
	var cpType, status, createdAt string
	err := rows.Scan(&cp.ID, &cp.SessionID, &cp.ParentID, &cpType, &status,
		&cp.Label, &cp.OverlayPath, &cp.VMStatePath, &cp.MetadataPath, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("scan checkpoint row: %w", err)
	}
	cp.Type = loka.CheckpointType(cpType)
	cp.Status = loka.CheckpointStatus(status)
	cp.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &cp, nil
}

var _ store.CheckpointRepository = (*checkpointRepo)(nil)
