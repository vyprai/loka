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
	// Enable incremental auto-vacuum so that SQLite reclaims free pages
	// automatically without the cost/lock of a full VACUUM.
	if _, err := db.Exec("PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set auto_vacuum: %w", err)
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

// DB returns the underlying sql.DB for operations that need direct access
// (e.g., running PRAGMA optimize after GC sweeps).
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

CREATE TABLE IF NOT EXISTS services (
	id               TEXT PRIMARY KEY,
	name             TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL DEFAULT 'deploying',
	worker_id        TEXT NOT NULL DEFAULT '',
	image_ref        TEXT NOT NULL DEFAULT '',
	image_id         TEXT NOT NULL DEFAULT '',
	recipe_name      TEXT NOT NULL DEFAULT '',
	command          TEXT NOT NULL DEFAULT '',
	args             TEXT NOT NULL DEFAULT '[]',
	env              TEXT NOT NULL DEFAULT '{}',
	workdir          TEXT NOT NULL DEFAULT '',
	port             INTEGER NOT NULL DEFAULT 0,
	vcpus            INTEGER NOT NULL DEFAULT 1,
	memory_mb        INTEGER NOT NULL DEFAULT 512,
	routes           TEXT NOT NULL DEFAULT '[]',
	bundle_key       TEXT NOT NULL DEFAULT '',
	idle_timeout     INTEGER NOT NULL DEFAULT 0,
	health_path      TEXT NOT NULL DEFAULT '',
	health_interval  INTEGER NOT NULL DEFAULT 0,
	health_timeout   INTEGER NOT NULL DEFAULT 0,
	health_retries   INTEGER NOT NULL DEFAULT 0,
	labels           TEXT NOT NULL DEFAULT '{}',
	mounts           TEXT NOT NULL DEFAULT '[]',
	autoscale        TEXT NOT NULL DEFAULT 'null',
	snapshot_id      TEXT NOT NULL DEFAULT '',
	ready            INTEGER NOT NULL DEFAULT 0,
	status_message   TEXT NOT NULL DEFAULT '',
	last_activity    TEXT NOT NULL DEFAULT (datetime('now')),
	created_at       TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_services_worker ON services(worker_id);
CREATE INDEX IF NOT EXISTS idx_services_status ON services(status);
CREATE INDEX IF NOT EXISTS idx_services_status_updated ON services(status, updated_at);

CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_worker ON sessions(worker_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status_updated ON sessions(status, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_name ON sessions(name) WHERE name != '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_services_name ON services(name) WHERE name != '';

CREATE INDEX IF NOT EXISTS idx_workers_status ON workers(status);

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
