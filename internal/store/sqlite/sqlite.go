package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vyprai/loka/internal/store"
)

func init() {
	store.RegisterFactory("sqlite", func(dsn string) (store.Store, error) {
		return New(dsn)
	})
}

// Store implements store.Store backed by SQLite.
// Uses separate read/write pools: writes go through a single connection
// (SQLite serializes writes), reads use a parallel pool with WAL mode.
type Store struct {
	db     *sql.DB // Write pool (MaxOpenConns=1, serialized)
	readDB *sql.DB // Read pool (MaxOpenConns=N, parallel via WAL)
}

// New creates a new SQLite store with separate read/write connection pools.
// For in-memory databases (:memory:), both pools share a single connection
// because each :memory: open creates a distinct database.
func New(dsn string) (*Store, error) {
	// Build DSN with pragmas that apply to ALL connections.
	pragmas := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	fullDSN := dsn + sep + pragmas

	// Writer: single connection, serialized writes. No SQLITE_BUSY possible.
	writeDB, err := sql.Open("sqlite", fullDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite (write): %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0) // Don't expire the writer.
	writeDB.Exec("PRAGMA auto_vacuum=INCREMENTAL")

	// In-memory databases: each sql.Open(":memory:") creates a separate DB,
	// so the read pool must share the same handle as the writer.
	if dsn == ":memory:" {
		return &Store{db: writeDB, readDB: writeDB}, nil
	}

	// Reader: multiple connections for parallel reads via WAL.
	readDB, err := sql.Open("sqlite", fullDSN+"&_pragma=query_only(ON)")
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("open sqlite (read): %w", err)
	}
	readDB.SetMaxOpenConns(8)
	readDB.SetMaxIdleConns(4)
	readDB.SetConnMaxLifetime(5 * time.Minute)

	return &Store{db: writeDB, readDB: readDB}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Incremental migrations for existing databases.
	for _, m := range migrations {
		s.db.ExecContext(ctx, m) // Ignore errors (column may already exist).
	}
	return nil
}

// migrations contains ALTER TABLE statements for columns added after the
// initial schema. Each statement is idempotent — SQLite returns an error
// if the column already exists, which we silently ignore.
var migrations = []string{
	`ALTER TABLE services ADD COLUMN forward_port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE services ADD COLUMN app_snapshot_mem TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE services ADD COLUMN app_snapshot_state TEXT NOT NULL DEFAULT ''`,
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
	// Volume revamp: block/object modes, worker placement, size limits.
	`ALTER TABLE volumes ADD COLUMN type TEXT NOT NULL DEFAULT 'block'`,
	`ALTER TABLE volumes ADD COLUMN status TEXT NOT NULL DEFAULT 'healthy'`,
	`ALTER TABLE volumes ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE volumes ADD COLUMN max_size_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE volumes ADD COLUMN primary_worker_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE volumes ADD COLUMN replica_worker_ids TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE volumes ADD COLUMN desired_replicas INTEGER NOT NULL DEFAULT 2`,
	`ALTER TABLE volumes ADD COLUMN bucket TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE volumes ADD COLUMN prefix TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE volumes ADD COLUMN region TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE volumes ADD COLUMN credentials TEXT NOT NULL DEFAULT ''`,
}

// DB returns the underlying sql.DB for operations that need direct access
// (e.g., running PRAGMA optimize after GC sweeps).
func (s *Store) DB() *sql.DB {
	return s.db
}

// ReadDB returns the read-only connection pool for operations that need
// direct read access (e.g., health checks).
func (s *Store) ReadDB() *sql.DB {
	return s.readDB
}

func (s *Store) Close() error {
	if s.readDB != s.db { // Don't double-close when both point to the same DB (tests).
		if err := s.readDB.Close(); err != nil {
			return err
		}
	}
	return s.db.Close()
}

func (s *Store) Sessions() store.SessionRepository      { return &sessionRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Executions() store.ExecutionRepository   { return &executionRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Checkpoints() store.CheckpointRepository { return &checkpointRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Workers() store.WorkerRepository         { return &workerRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Tokens() store.TokenRepository           { return &tokenRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Services() store.ServiceRepository       { return &serviceRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Volumes() store.VolumeRepository         { return &volumeRepo{db: s.db, readDB: s.readDB} }
func (s *Store) Tasks() store.TaskRepository             { return &taskRepo{db: s.db, readDB: s.readDB} }

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
	app_snapshot_mem   TEXT NOT NULL DEFAULT '',
	app_snapshot_state TEXT NOT NULL DEFAULT '',
	forward_port     INTEGER NOT NULL DEFAULT 0,
	ready            INTEGER NOT NULL DEFAULT 0,
	status_message   TEXT NOT NULL DEFAULT '',
	database_config  TEXT NOT NULL DEFAULT '',
	uses             TEXT NOT NULL DEFAULT '{}',
	parent_service_id TEXT NOT NULL DEFAULT '',
	replicas         INTEGER NOT NULL DEFAULT 0,
	relation_type    TEXT NOT NULL DEFAULT '',
	last_activity    TEXT NOT NULL DEFAULT (datetime('now')),
	created_at       TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_services_worker ON services(worker_id);
CREATE INDEX IF NOT EXISTS idx_services_status ON services(status);
CREATE INDEX IF NOT EXISTS idx_services_status_updated ON services(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_services_database ON services(database_config) WHERE database_config != '';
CREATE INDEX IF NOT EXISTS idx_services_name ON services(name) WHERE name != '';

CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_worker ON sessions(worker_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status_updated ON sessions(status, updated_at);
-- Deduplicate session names before adding unique constraint.
-- Append short ID suffix to duplicate non-empty names.
UPDATE sessions SET name = name || '-' || substr(id, 1, 4)
  WHERE name != '' AND rowid NOT IN (
    SELECT min(rowid) FROM sessions WHERE name != '' GROUP BY name
  );
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

CREATE TABLE IF NOT EXISTS volumes (
	name              TEXT PRIMARY KEY,
	type              TEXT NOT NULL DEFAULT 'block',
	status            TEXT NOT NULL DEFAULT 'healthy',
	provider          TEXT NOT NULL DEFAULT 'volume',
	size_bytes        INTEGER NOT NULL DEFAULT 0,
	max_size_bytes    INTEGER NOT NULL DEFAULT 0,
	primary_worker_id TEXT NOT NULL DEFAULT '',
	replica_worker_ids TEXT NOT NULL DEFAULT '[]',
	desired_replicas  INTEGER NOT NULL DEFAULT 2,
	mount_count       INTEGER NOT NULL DEFAULT 0,
	bucket            TEXT NOT NULL DEFAULT '',
	prefix            TEXT NOT NULL DEFAULT '',
	region            TEXT NOT NULL DEFAULT '',
	credentials       TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS tasks (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL DEFAULT 'pending',
	exit_code      INTEGER NOT NULL DEFAULT 0,
	worker_id      TEXT NOT NULL DEFAULT '',
	image_ref      TEXT NOT NULL DEFAULT '',
	command        TEXT NOT NULL DEFAULT '',
	args           TEXT NOT NULL DEFAULT '[]',
	env            TEXT NOT NULL DEFAULT '{}',
	workdir        TEXT NOT NULL DEFAULT '',
	bundle_key     TEXT NOT NULL DEFAULT '',
	vcpus          INTEGER NOT NULL DEFAULT 1,
	memory_mb      INTEGER NOT NULL DEFAULT 512,
	mounts         TEXT NOT NULL DEFAULT '[]',
	timeout        INTEGER NOT NULL DEFAULT 0,
	status_message TEXT NOT NULL DEFAULT '',
	started_at     TEXT NOT NULL DEFAULT '',
	completed_at   TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_name ON tasks(name);
`
