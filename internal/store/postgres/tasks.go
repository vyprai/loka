package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type taskRepo struct {
	db *sql.DB
}

func (r *taskRepo) Create(ctx context.Context, t *loka.Task) error {
	return fmt.Errorf("postgres task repo not implemented")
}

func (r *taskRepo) Get(ctx context.Context, id string) (*loka.Task, error) {
	return nil, fmt.Errorf("postgres task repo not implemented")
}

func (r *taskRepo) Update(ctx context.Context, t *loka.Task) error {
	return fmt.Errorf("postgres task repo not implemented")
}

func (r *taskRepo) Delete(ctx context.Context, id string) error {
	return fmt.Errorf("postgres task repo not implemented")
}

func (r *taskRepo) List(ctx context.Context, f store.TaskFilter) ([]*loka.Task, error) {
	return nil, fmt.Errorf("postgres task repo not implemented")
}
