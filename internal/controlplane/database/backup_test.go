package database

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
)

func newTestBackupManager(t *testing.T) *BackupManager {
	t.Helper()
	dir := t.TempDir()
	obj, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// Don't start the background scheduler for tests — create manually.
	ctx, cancel := context.WithCancel(context.Background())
	m := &BackupManager{
		store:    nil, // Not needed for direct backup tests.
		objStore: obj,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
	}
	t.Cleanup(func() { cancel() })
	return m
}

func testDB(engine, name string) *loka.Service {
	return &loka.Service{
		ID:   "svc-" + name,
		Name: name,
		Port: 5432,
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:    engine,
			Version:   "16",
			LoginRole: "testuser",
			Password:  "testpass",
			DBName:    name,
			Role:      loka.DatabaseRolePrimary,
			Backup: &loka.BackupConfig{
				Enabled:   true,
				Schedule:  "0 */6 * * *",
				Retention: 7,
				WAL:       true,
			},
		},
	}
}

func TestCreateBackup_Postgres(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "pg-backup-test")

	chainID, err := m.CreateBackup(context.Background(), db)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if chainID == "" {
		t.Fatal("expected non-empty chain ID")
	}

	// Verify catalog was written.
	catalog, err := m.loadCatalog(context.Background(), db.Name)
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if len(catalog.Chains) != 1 {
		t.Fatalf("expected 1 chain, got %d", len(catalog.Chains))
	}
	if catalog.Chains[0].ID != chainID {
		t.Errorf("chain ID = %q, want %q", catalog.Chains[0].ID, chainID)
	}
	if catalog.Chains[0].Engine != "postgres" {
		t.Errorf("engine = %q, want postgres", catalog.Chains[0].Engine)
	}
	if catalog.Chains[0].Status != "complete" {
		t.Errorf("status = %q, want complete", catalog.Chains[0].Status)
	}
}

func TestCreateBackup_MySQL(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("mysql", "my-backup-test")
	db.DatabaseConfig.Engine = "mysql"

	chainID, err := m.CreateBackup(context.Background(), db)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if chainID == "" {
		t.Fatal("expected non-empty chain ID")
	}
}

func TestCreateBackup_Redis(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("redis", "rd-backup-test")
	db.DatabaseConfig.Engine = "redis"

	chainID, err := m.CreateBackup(context.Background(), db)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if chainID == "" {
		t.Fatal("expected non-empty chain ID")
	}
}

func TestCreateBackup_UnsupportedEngine(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("mongodb", "bad-engine")
	db.DatabaseConfig.Engine = "mongodb"

	_, err := m.CreateBackup(context.Background(), db)
	if err == nil {
		t.Fatal("expected error for unsupported engine")
	}
}

func TestCreateBackup_NilConfig(t *testing.T) {
	m := newTestBackupManager(t)
	svc := &loka.Service{ID: "svc-1", Name: "no-config"}

	_, err := m.CreateBackup(context.Background(), svc)
	if err == nil {
		t.Fatal("expected error for nil DatabaseConfig")
	}
}

func TestListBackups_Empty(t *testing.T) {
	m := newTestBackupManager(t)

	catalog, err := m.ListBackups(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(catalog.Chains) != 0 {
		t.Errorf("expected 0 chains, got %d", len(catalog.Chains))
	}
}

func TestListBackups_WithChains(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "list-test")

	m.CreateBackup(context.Background(), db)
	m.CreateBackup(context.Background(), db)

	catalog, err := m.ListBackups(context.Background(), db.Name)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(catalog.Chains) != 2 {
		t.Errorf("expected 2 chains, got %d", len(catalog.Chains))
	}
}

func TestGetBackup_Found(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "get-test")

	chainID, _ := m.CreateBackup(context.Background(), db)

	chain, err := m.GetBackup(context.Background(), db.Name, chainID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if chain.ID != chainID {
		t.Errorf("ID = %q, want %q", chain.ID, chainID)
	}
}

func TestGetBackup_NotFound(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "get-missing")

	m.CreateBackup(context.Background(), db)

	_, err := m.GetBackup(context.Background(), db.Name, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent backup ID")
	}
}

func TestEnforceRetention(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "retention-test")

	// Create a backup, then set its creation time to the past.
	m.CreateBackup(context.Background(), db)

	mu := m.catalogLock(db.Name)
	mu.Lock()
	catalog, _ := m.loadCatalog(context.Background(), db.Name)
	catalog.Chains[0].CreatedAt = time.Now().AddDate(0, 0, -10) // 10 days ago
	m.saveCatalog(context.Background(), db.Name, catalog)
	mu.Unlock()

	// Create a second (recent) backup.
	m.CreateBackup(context.Background(), db)

	// Enforce retention with 7 days → old chain should be deleted.
	mu.Lock()
	catalog, _ = m.loadCatalog(context.Background(), db.Name)
	m.enforceRetention(context.Background(), db.Name, catalog, 7)
	m.saveCatalog(context.Background(), db.Name, catalog)
	mu.Unlock()

	catalog, _ = m.loadCatalog(context.Background(), db.Name)
	if len(catalog.Chains) != 1 {
		t.Errorf("expected 1 chain after retention, got %d", len(catalog.Chains))
	}
}

func TestEnforceRetention_KeepsRecent(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "retention-keep")

	m.CreateBackup(context.Background(), db)

	mu := m.catalogLock(db.Name)
	mu.Lock()
	catalog, _ := m.loadCatalog(context.Background(), db.Name)
	m.enforceRetention(context.Background(), db.Name, catalog, 7)
	m.saveCatalog(context.Background(), db.Name, catalog)
	mu.Unlock()

	catalog, _ = m.loadCatalog(context.Background(), db.Name)
	if len(catalog.Chains) != 1 {
		t.Errorf("expected 1 chain (recent, not deleted), got %d", len(catalog.Chains))
	}
}

func TestIsBackupDue_NoCatalog(t *testing.T) {
	m := newTestBackupManager(t)
	cfg := &loka.BackupConfig{Schedule: "0 */6 * * *"}

	if !m.isBackupDue(nil, cfg) {
		t.Error("expected backup due when no catalog exists")
	}
}

func TestIsBackupDue_Recent(t *testing.T) {
	m := newTestBackupManager(t)
	cfg := &loka.BackupConfig{Schedule: "0 */6 * * *"}
	catalog := &BackupCatalog{LastFull: time.Now().Add(-1 * time.Hour)}

	if m.isBackupDue(catalog, cfg) {
		t.Error("expected backup NOT due (1h ago, 6h schedule)")
	}
}

func TestIsBackupDue_Overdue(t *testing.T) {
	m := newTestBackupManager(t)
	cfg := &loka.BackupConfig{Schedule: "0 */6 * * *"}
	catalog := &BackupCatalog{LastFull: time.Now().Add(-7 * time.Hour)}

	if !m.isBackupDue(catalog, cfg) {
		t.Error("expected backup due (7h ago, 6h schedule)")
	}
}

func TestLoadCatalog_MalformedJSON(t *testing.T) {
	m := newTestBackupManager(t)

	// Write garbage JSON to the catalog location.
	key := "corrupt-db/meta.json"
	garbage := "{not valid json!!!"
	m.objStore.Put(context.Background(), backupBucket, key, strings.NewReader(garbage), int64(len(garbage)))

	catalog, err := m.loadCatalog(context.Background(), "corrupt-db")
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if catalog != nil {
		t.Error("expected nil catalog on error")
	}
}

func TestLoadCatalog_NonExistent(t *testing.T) {
	m := newTestBackupManager(t)
	catalog, err := m.loadCatalog(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error for non-existent catalog")
	}
	if catalog != nil {
		t.Error("expected nil catalog")
	}
}

func TestCatalogLocking(t *testing.T) {
	m := newTestBackupManager(t)
	db := testDB("postgres", "lock-test")

	// Run 10 concurrent backups — catalog should not corrupt.
	var wg sync.WaitGroup
	errors := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.CreateBackup(context.Background(), db); err != nil {
				errors <- err
			}
		}()
	}
	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent backup failed: %v", err)
	}

	catalog, _ := m.ListBackups(context.Background(), db.Name)
	if len(catalog.Chains) != 10 {
		t.Errorf("expected 10 chains from concurrent writes, got %d", len(catalog.Chains))
	}
}
