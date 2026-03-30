package postgres

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/vyprai/loka/internal/store"
)

func init() {
	store.RegisterFactory("postgres", func(dsn string) (store.Store, error) {
		return New(dsn)
	})
}

// Store implements store.Store backed by PostgreSQL.
type Store struct {
	db *sql.DB
}

// New creates a new PostgreSQL store.
func New(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	return &Store{db: db}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Incremental migrations for columns added after the initial schema.
	for _, m := range migrations {
		s.db.ExecContext(ctx, m) // Ignore errors (column may already exist).
	}
	return nil
}

var migrations = []string{
	`ALTER TABLE services ADD COLUMN IF NOT EXISTS forward_port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE services ADD COLUMN IF NOT EXISTS app_snapshot_mem TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE services ADD COLUMN IF NOT EXISTS app_snapshot_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE services ADD COLUMN IF NOT EXISTS database_config TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE services ADD COLUMN IF NOT EXISTS uses TEXT NOT NULL DEFAULT '{}'`,
	`CREATE TABLE IF NOT EXISTS file_locks (
		lock_key    TEXT PRIMARY KEY,
		volume      TEXT NOT NULL,
		path        TEXT NOT NULL,
		worker_id   TEXT NOT NULL,
		exclusive   INTEGER NOT NULL DEFAULT 1,
		acquired_at TEXT NOT NULL,
		expires_at  TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_file_locks_volume ON file_locks(volume)`,
	`CREATE INDEX IF NOT EXISTS idx_file_locks_expires ON file_locks(expires_at)`,
}

// DB returns the underlying *sql.DB for components that need raw access.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Sessions() store.SessionRepository      { return &sessionRepo{db: s.db} }
func (s *Store) Executions() store.ExecutionRepository   { return &executionRepo{db: s.db} }
func (s *Store) Checkpoints() store.CheckpointRepository { return &checkpointRepo{db: s.db} }
func (s *Store) Workers() store.WorkerRepository         { return &workerRepo{db: s.db} }
func (s *Store) Tokens() store.TokenRepository           { return &tokenRepo{db: s.db} }
func (s *Store) Services() store.ServiceRepository       { return &serviceRepo{db: s.db} }
func (s *Store) Volumes() store.VolumeRepository         { return &volumeRepo{db: s.db} }
func (s *Store) Tasks() store.TaskRepository             { return &taskRepo{db: s.db} }

var _ store.Store = (*Store)(nil)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'creating',
	mode       TEXT NOT NULL DEFAULT 'inspect',
	worker_id  TEXT NOT NULL DEFAULT '',
	snapshot   TEXT NOT NULL DEFAULT '',
	vcpus      INTEGER NOT NULL DEFAULT 1,
	memory_mb  INTEGER NOT NULL DEFAULT 512,
	labels     JSONB NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS executions (
	id         TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	status     TEXT NOT NULL DEFAULT 'pending',
	parallel   BOOLEAN NOT NULL DEFAULT FALSE,
	commands   JSONB NOT NULL DEFAULT '[]',
	results    JSONB NOT NULL DEFAULT '[]',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_executions_session ON executions(session_id);

CREATE TABLE IF NOT EXISTS checkpoints (
	id            TEXT PRIMARY KEY,
	session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	parent_id     TEXT NOT NULL DEFAULT '',
	type          TEXT NOT NULL DEFAULT 'light',
	status        TEXT NOT NULL DEFAULT 'creating',
	label         TEXT NOT NULL DEFAULT '',
	overlay_path  TEXT NOT NULL DEFAULT '',
	vmstate_path  TEXT NOT NULL DEFAULT '',
	metadata_path TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_session ON checkpoints(session_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_name ON sessions(name) WHERE name != '';

CREATE TABLE IF NOT EXISTS workers (
	id            TEXT PRIMARY KEY,
	hostname      TEXT NOT NULL DEFAULT '',
	ip_address    TEXT NOT NULL DEFAULT '',
	provider      TEXT NOT NULL DEFAULT '',
	region        TEXT NOT NULL DEFAULT '',
	zone          TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL DEFAULT 'registering',
	labels        JSONB NOT NULL DEFAULT '{}',
	capacity_cpu  INTEGER NOT NULL DEFAULT 0,
	capacity_mem  BIGINT NOT NULL DEFAULT 0,
	capacity_disk BIGINT NOT NULL DEFAULT 0,
	agent_version TEXT NOT NULL DEFAULT '',
	kvm_available BOOLEAN NOT NULL DEFAULT FALSE,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_seen     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS worker_tokens (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	token      TEXT NOT NULL UNIQUE,
	expires_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01',
	used       BOOLEAN NOT NULL DEFAULT FALSE,
	worker_id  TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS volumes (
	name        TEXT PRIMARY KEY,
	provider    TEXT NOT NULL DEFAULT 'volume',
	mount_count INTEGER NOT NULL DEFAULT 0,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
