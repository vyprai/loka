package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/vyprai/loka/internal/store"
)

func init() {
	store.RegisterFactory("sqlite", func(dsn string) (store.Store, error) {
		return New(dsn)
	})
}

// Store implements store.Store backed by SQLite.
type Store struct {
	db *sql.DB
}

// New creates a new SQLite store.
func New(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Enable WAL mode for better concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Sessions() store.SessionRepository    { return &sessionRepo{db: s.db} }
func (s *Store) Executions() store.ExecutionRepository { return &executionRepo{db: s.db} }
func (s *Store) Checkpoints() store.CheckpointRepository { return &checkpointRepo{db: s.db} }
func (s *Store) Workers() store.WorkerRepository      { return &workerRepo{db: s.db} }
func (s *Store) Tokens() store.TokenRepository        { return &tokenRepo{db: s.db} }

var _ store.Store = (*Store)(nil)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT 'creating',
	mode        TEXT NOT NULL DEFAULT 'inspect',
	worker_id   TEXT NOT NULL DEFAULT '',
	image_ref   TEXT NOT NULL DEFAULT '',
	image_id    TEXT NOT NULL DEFAULT '',
	snapshot_id TEXT NOT NULL DEFAULT '',
	vcpus       INTEGER NOT NULL DEFAULT 1,
	memory_mb   INTEGER NOT NULL DEFAULT 512,
	labels      TEXT NOT NULL DEFAULT '{}',
	exec_policy TEXT NOT NULL DEFAULT '{}',
	created_at  TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS executions (
	id         TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	status     TEXT NOT NULL DEFAULT 'pending',
	parallel   INTEGER NOT NULL DEFAULT 0,
	commands   TEXT NOT NULL DEFAULT '[]',
	results    TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_executions_session ON executions(session_id);

CREATE TABLE IF NOT EXISTS checkpoints (
	id           TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	parent_id    TEXT NOT NULL DEFAULT '',
	type         TEXT NOT NULL DEFAULT 'light',
	status       TEXT NOT NULL DEFAULT 'creating',
	label        TEXT NOT NULL DEFAULT '',
	overlay_path TEXT NOT NULL DEFAULT '',
	vmstate_path TEXT NOT NULL DEFAULT '',
	metadata_path TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_session ON checkpoints(session_id);

CREATE TABLE IF NOT EXISTS workers (
	id            TEXT PRIMARY KEY,
	hostname      TEXT NOT NULL DEFAULT '',
	ip_address    TEXT NOT NULL DEFAULT '',
	provider      TEXT NOT NULL DEFAULT '',
	region        TEXT NOT NULL DEFAULT '',
	zone          TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL DEFAULT 'registering',
	labels        TEXT NOT NULL DEFAULT '{}',
	capacity_cpu  INTEGER NOT NULL DEFAULT 0,
	capacity_mem  INTEGER NOT NULL DEFAULT 0,
	capacity_disk INTEGER NOT NULL DEFAULT 0,
	agent_version TEXT NOT NULL DEFAULT '',
	kvm_available INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
	last_seen     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS worker_tokens (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	token      TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL DEFAULT '',
	used       INTEGER NOT NULL DEFAULT 0,
	worker_id  TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`
