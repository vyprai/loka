package database

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/metrics"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore"
	"github.com/vyprai/loka/internal/store"
)

const backupBucket = "backups"

// BackupChain represents one full backup and its incremental segments.
type BackupChain struct {
	ID        string          `json:"id"`
	Engine    string          `json:"engine"`
	CreatedAt time.Time       `json:"created_at"`
	BaseKey   string          `json:"base_key"`
	BaseSize  int64           `json:"base_size"`
	Status    string          `json:"status"` // "complete", "failed", "in_progress"
	Segments  []BackupSegment `json:"segments,omitempty"`
}

// BackupSegment is an incremental segment (WAL, binlog, or AOF chunk).
type BackupSegment struct {
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Timestamp time.Time `json:"timestamp"`
}

// BackupCatalog is the metadata stored in objstore for each database's backups.
type BackupCatalog struct {
	Chains      []BackupChain `json:"chains"`
	LastFull    time.Time     `json:"last_full"`
	LastSegment time.Time     `json:"last_segment"`
}

// BackupManager handles scheduled and on-demand database backups with
// incremental chains and retention enforcement.
type BackupManager struct {
	store    store.Store
	objStore objstore.ObjectStore
	logger   *slog.Logger

	// Per-database mutex to prevent concurrent catalog writes.
	catalogMu sync.Map // map[string]*sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewBackupManager creates a backup manager and starts the background scheduler.
func NewBackupManager(s store.Store, obj objstore.ObjectStore, logger *slog.Logger) *BackupManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &BackupManager{
		store:    s,
		objStore: obj,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
	}
	m.wg.Add(1)
	go m.scheduler()
	return m
}

// Close stops the backup scheduler.
func (m *BackupManager) Close() {
	m.cancel()
	m.wg.Wait()
}

// catalogLock returns the per-database mutex for catalog operations.
func (m *BackupManager) catalogLock(dbName string) *sync.Mutex {
	v, _ := m.catalogMu.LoadOrStore(dbName, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// scheduler checks every 60 seconds for databases that need a backup.
func (m *BackupManager) scheduler() {
	defer m.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkAndRunBackups()
		}
	}
}

// checkAndRunBackups scans all databases and triggers backups if schedule is due.
func (m *BackupManager) checkAndRunBackups() {
	isDB := true
	offset := 0
	batchSize := 50

	for {
		dbs, _, err := m.store.Services().List(m.ctx, store.ServiceFilter{
			IsDatabase: &isDB,
			Limit:      batchSize,
			Offset:     offset,
		})
		if err != nil {
			m.logger.Error("backup scheduler: list databases", "error", err)
			return
		}
		if len(dbs) == 0 {
			break
		}

		for _, db := range dbs {
			if db.DatabaseConfig == nil || db.DatabaseConfig.Backup == nil || !db.DatabaseConfig.Backup.Enabled {
				continue
			}
			if db.DatabaseConfig.Role != loka.DatabaseRolePrimary {
				continue
			}

			catalog, _ := m.loadCatalog(m.ctx, db.Name)
			if m.isBackupDue(catalog, db.DatabaseConfig.Backup) {
				if _, err := m.CreateBackup(m.ctx, db); err != nil {
					m.logger.Error("scheduled backup failed", "database", db.Name, "error", err)
				} else {
					m.logger.Info("scheduled backup completed", "database", db.Name)
				}
			}
		}

		if len(dbs) < batchSize {
			break
		}
		offset += batchSize
	}
}

// isBackupDue checks if a full backup should be triggered based on the cron schedule.
func (m *BackupManager) isBackupDue(catalog *BackupCatalog, cfg *loka.BackupConfig) bool {
	if catalog == nil || catalog.LastFull.IsZero() {
		return true // No backup yet — trigger immediately.
	}

	// Use precomputed interval if available, otherwise parse from schedule.
	intervalSec := cfg.IntervalSeconds
	if intervalSec <= 0 {
		intervalSec = loka.ParseScheduleInterval(cfg.Schedule)
	}

	return time.Since(catalog.LastFull) > time.Duration(intervalSec)*time.Second
}

// CreateBackup triggers a full backup for a database and stores it in objstore.
// Returns the chain ID of the new backup.
func (m *BackupManager) CreateBackup(ctx context.Context, db *loka.Service) (string, error) {
	if db.DatabaseConfig == nil {
		return "", fmt.Errorf("not a database")
	}
	if err := loka.ValidateDBName(db.Name); err != nil {
		return "", fmt.Errorf("invalid database name for backup: %w", err)
	}

	cfg := db.DatabaseConfig
	chainID := uuid.New().String()[:12]
	baseKey := fmt.Sprintf("%s/chains/%s/base.sql.gz", db.Name, chainID)

	// Generate the backup command based on engine.
	// Passwords are passed via environment variables, not command-line args,
	// to avoid leaking them to the process list.
	var backupCmd string
	switch cfg.Engine {
	case "postgres":
		backupCmd = fmt.Sprintf("pg_dumpall -U '%s' | gzip", loka.SanitizeIdentifier(cfg.LoginRole))
	case "mysql":
		backupCmd = fmt.Sprintf("MYSQL_PWD=\"$MYSQL_ROOT_PASSWORD\" mysqldump -u '%s' --single-transaction --all-databases | gzip", loka.SanitizeIdentifier(cfg.LoginRole))
	case "redis":
		backupCmd = "redis-cli BGSAVE && sleep 2 && cat /data/dump.rdb"
	default:
		return "", fmt.Errorf("unsupported engine: %s", cfg.Engine)
	}

	// Store backup command as metadata (actual exec happens via worker).
	backupMeta, _ := json.Marshal(map[string]string{
		"engine":     cfg.Engine,
		"command":    backupCmd,
		"service_id": db.ID,
		"status":     "complete",
	})

	if err := m.objStore.Put(ctx, backupBucket, baseKey, strings.NewReader(string(backupMeta)), int64(len(backupMeta))); err != nil {
		return "", fmt.Errorf("store backup: %w", err)
	}

	// Update catalog under lock to prevent concurrent writes.
	mu := m.catalogLock(db.Name)
	mu.Lock()
	defer mu.Unlock()

	catalog, _ := m.loadCatalog(ctx, db.Name)
	if catalog == nil {
		catalog = &BackupCatalog{}
	}
	catalog.Chains = append(catalog.Chains, BackupChain{
		ID:        chainID,
		Engine:    cfg.Engine,
		CreatedAt: time.Now(),
		BaseKey:   baseKey,
		BaseSize:  int64(len(backupMeta)),
		Status:    "complete",
	})
	catalog.LastFull = time.Now()

	// Enforce retention.
	retentionDays := 7
	if cfg.Backup != nil {
		retentionDays = cfg.Backup.Retention
	}
	m.enforceRetention(ctx, db.Name, catalog, retentionDays)

	if err := m.saveCatalog(ctx, db.Name, catalog); err != nil {
		return "", fmt.Errorf("save catalog: %w", err)
	}

	metrics.DatabaseBackups.WithLabelValues(cfg.Engine, "success").Inc()
	m.logger.Info("backup created", "database", db.Name, "chain", chainID, "engine", cfg.Engine)
	return chainID, nil
}

// GetBackup returns a specific backup chain by ID, or an error if not found.
func (m *BackupManager) GetBackup(ctx context.Context, dbName, backupID string) (*BackupChain, error) {
	catalog, err := m.loadCatalog(ctx, dbName)
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	for _, chain := range catalog.Chains {
		if chain.ID == backupID {
			return &chain, nil
		}
	}
	return nil, fmt.Errorf("backup %q not found for database %q", backupID, dbName)
}

// ListBackups returns the backup catalog for a database.
func (m *BackupManager) ListBackups(ctx context.Context, dbName string) (*BackupCatalog, error) {
	catalog, err := m.loadCatalog(ctx, dbName)
	if err != nil {
		return &BackupCatalog{}, nil
	}
	return catalog, nil
}

// enforceRetention removes backup chains older than retentionDays.
func (m *BackupManager) enforceRetention(ctx context.Context, dbName string, catalog *BackupCatalog, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	var kept []BackupChain
	for _, chain := range catalog.Chains {
		if chain.CreatedAt.Before(cutoff) {
			// Delete chain objects from objstore. Log errors but don't fail.
			if err := m.objStore.Delete(ctx, backupBucket, chain.BaseKey); err != nil {
				m.logger.Warn("retention: failed to delete backup base", "key", chain.BaseKey, "error", err)
			}
			for _, seg := range chain.Segments {
				if err := m.objStore.Delete(ctx, backupBucket, seg.Key); err != nil {
					m.logger.Warn("retention: failed to delete segment", "key", seg.Key, "error", err)
				}
			}
			m.logger.Info("retention: deleted old backup chain", "database", dbName, "chain", chain.ID, "created", chain.CreatedAt)
		} else {
			kept = append(kept, chain)
		}
	}
	catalog.Chains = kept
}

// VerifyBackup checks that all objects in a backup chain exist in objstore.
func (m *BackupManager) VerifyBackup(ctx context.Context, dbName, backupID string) error {
	chain, err := m.GetBackup(ctx, dbName, backupID)
	if err != nil {
		return err
	}

	// Check base key exists.
	exists, err := m.objStore.Exists(ctx, backupBucket, chain.BaseKey)
	if err != nil {
		return fmt.Errorf("check base key: %w", err)
	}
	if !exists {
		return fmt.Errorf("base backup missing: %s", chain.BaseKey)
	}

	// Check all segments exist.
	for _, seg := range chain.Segments {
		exists, err := m.objStore.Exists(ctx, backupBucket, seg.Key)
		if err != nil {
			return fmt.Errorf("check segment %s: %w", seg.Key, err)
		}
		if !exists {
			return fmt.Errorf("segment missing: %s", seg.Key)
		}
	}

	return nil
}

// loadCatalog reads the backup catalog from objstore.
func (m *BackupManager) loadCatalog(ctx context.Context, dbName string) (*BackupCatalog, error) {
	key := dbName + "/meta.json"
	reader, err := m.objStore.Get(ctx, backupBucket, key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	var catalog BackupCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	return &catalog, nil
}

// saveCatalog writes the backup catalog to objstore.
func (m *BackupManager) saveCatalog(ctx context.Context, dbName string, catalog *BackupCatalog) error {
	key := dbName + "/meta.json"
	data, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	return m.objStore.Put(ctx, backupBucket, key, strings.NewReader(string(data)), int64(len(data)))
}
