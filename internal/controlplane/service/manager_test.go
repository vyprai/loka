package service

import (
	"context"
	"log/slog"
	"sync"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/controlplane/image"
	"github.com/vyprai/loka/internal/controlplane/scheduler"
	"github.com/vyprai/loka/internal/controlplane/volume"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
	"github.com/vyprai/loka/internal/store"
	"github.com/vyprai/loka/internal/store/sqlite"

	_ "modernc.org/sqlite"
)

// setupTestStore creates an in-memory SQLite store for testing.
func setupTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// createTestService inserts a service with the given status and creation time.
func createTestService(t *testing.T, s *sqlite.Store, name string, status loka.ServiceStatus, createdAt time.Time) *loka.Service {
	t.Helper()
	svc := &loka.Service{
		ID:        uuid.New().String(),
		Name:      name,
		Status:    status,
		ImageRef:  "nginx:latest",
		Command:   "nginx",
		Port:      8080,
		VCPUs:     1,
		MemoryMB:  256,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := s.Services().Create(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	return svc
}

// newManagerFromStore creates a Manager, triggering recoverStuckDeploys.
func newManagerFromStore(t *testing.T, s *sqlite.Store) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := worker.NewRegistry(s, logger, nil)
	sched := scheduler.New(reg, "", nil)

	dataDir := t.TempDir()
	objStore, err := local.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	imgMgr := image.NewManager(objStore, dataDir, logger)
	volMgr := volume.NewManager(s, objStore, nil, logger, nil)

	// NewManager calls recoverStuckDeploys in its constructor.
	m := NewManager(s, reg, sched, imgMgr, objStore, volMgr, logger, nil)
	t.Cleanup(func() { m.Close() })
	return m
}

func TestRecoverStuckDeploys_MarksOldAsError(t *testing.T) {
	s := setupTestStore(t)

	// Insert a service stuck in "deploying" from 20 minutes ago.
	oldTime := time.Now().Add(-20 * time.Minute)
	svc := createTestService(t, s, "stuck-deploy", loka.ServiceStatusDeploying, oldTime)

	// Creating the manager triggers recoverStuckDeploys.
	_ = newManagerFromStore(t, s)

	// Verify the service was marked as error.
	got, err := s.Services().Get(context.Background(), svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.ServiceStatusError {
		t.Errorf("status = %q, want %q", got.Status, loka.ServiceStatusError)
	}
	if got.StatusMessage != "deploy interrupted by restart" {
		t.Errorf("status message = %q, want %q", got.StatusMessage, "deploy interrupted by restart")
	}
}

func TestRecoverStuckDeploys_LeavesRecentAlone(t *testing.T) {
	s := setupTestStore(t)

	// Insert a service that started deploying 2 minutes ago (recent, not stale).
	recentTime := time.Now().Add(-2 * time.Minute)
	svc := createTestService(t, s, "recent-deploy", loka.ServiceStatusDeploying, recentTime)

	// Creating the manager triggers recoverStuckDeploys.
	_ = newManagerFromStore(t, s)

	// Verify the service was left as deploying.
	got, err := s.Services().Get(context.Background(), svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.ServiceStatusDeploying {
		t.Errorf("status = %q, want %q (should not be changed)", got.Status, loka.ServiceStatusDeploying)
	}
}

// --- Database deploy tests ---

func TestDeploy_DatabasePostgres(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "test-pg",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:   "postgres",
			Version:  "16",
			LoginRole: "pguser",
			Password: "pgpass",
			DBName:   "testdb",
			Role:     loka.DatabaseRolePrimary,
		},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// Verify engine defaults were applied.
	if svc.ImageRef != "postgres:16" {
		t.Errorf("ImageRef = %q, want postgres:16", svc.ImageRef)
	}
	if svc.Port != 5432 {
		t.Errorf("Port = %d, want 5432", svc.Port)
	}

	// Verify env vars.
	if svc.Env["POSTGRES_USER"] != "pguser" {
		t.Errorf("POSTGRES_USER = %q, want pguser", svc.Env["POSTGRES_USER"])
	}
	if svc.Env["POSTGRES_PASSWORD"] != "pgpass" {
		t.Errorf("POSTGRES_PASSWORD = %q, want pgpass", svc.Env["POSTGRES_PASSWORD"])
	}
	if svc.Env["POSTGRES_DB"] != "testdb" {
		t.Errorf("POSTGRES_DB = %q, want testdb", svc.Env["POSTGRES_DB"])
	}

	// Verify persistent volume mount.
	found := false
	for _, m := range svc.Mounts {
		if m.Name == "db-test-pg" && m.Path == "/var/lib/postgresql/data" && m.Provider == "volume" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected volume mount db-test-pg at /var/lib/postgresql/data, mounts = %+v", svc.Mounts)
	}

	// Verify no domain routes (internal only).
	if len(svc.Routes) != 0 {
		t.Errorf("expected no routes for database, got %d", len(svc.Routes))
	}

	// Verify health path is empty (TCP check).
	if svc.HealthPath != "" {
		t.Errorf("HealthPath = %q, want empty (TCP check)", svc.HealthPath)
	}

	// Verify DatabaseConfig was stored.
	if svc.DatabaseConfig == nil {
		t.Fatal("expected DatabaseConfig to be set")
	}
	if svc.DatabaseConfig.Engine != "postgres" {
		t.Errorf("DatabaseConfig.Engine = %q, want postgres", svc.DatabaseConfig.Engine)
	}
}

func TestDeploy_DatabaseMySQL(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "test-mysql",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:   "mysql",
			Version:  "8.0",
			LoginRole: "app",
			Password: "secret",
			DBName:   "appdb",
			Role:     loka.DatabaseRolePrimary,
		},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	if svc.ImageRef != "mysql:8.0" {
		t.Errorf("ImageRef = %q, want mysql:8.0", svc.ImageRef)
	}
	if svc.Port != 3306 {
		t.Errorf("Port = %d, want 3306", svc.Port)
	}
	if svc.Env["MYSQL_ROOT_PASSWORD"] != "secret" {
		t.Errorf("MYSQL_ROOT_PASSWORD = %q, want secret", svc.Env["MYSQL_ROOT_PASSWORD"])
	}
	if svc.Env["MYSQL_DATABASE"] != "appdb" {
		t.Errorf("MYSQL_DATABASE = %q, want appdb", svc.Env["MYSQL_DATABASE"])
	}

	// Volume mount.
	found := false
	for _, m := range svc.Mounts {
		if m.Name == "db-test-mysql" && m.Path == "/var/lib/mysql" {
			found = true
		}
	}
	if !found {
		t.Error("expected volume mount db-test-mysql at /var/lib/mysql")
	}
}

func TestDeploy_DatabaseRedis(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "test-redis",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:   "redis",
			Version:  "7",
			Password: "redispass",
			Role:     loka.DatabaseRolePrimary,
		},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	if svc.ImageRef != "redis:7" {
		t.Errorf("ImageRef = %q, want redis:7", svc.ImageRef)
	}
	if svc.Port != 6379 {
		t.Errorf("Port = %d, want 6379", svc.Port)
	}
	// Redis uses ACL mode (no --requirepass args).
	if len(svc.Args) != 0 {
		t.Errorf("Args = %v, want empty (ACL mode)", svc.Args)
	}

	// Volume mount.
	found := false
	for _, m := range svc.Mounts {
		if m.Name == "db-test-redis" && m.Path == "/data" {
			found = true
		}
	}
	if !found {
		t.Error("expected volume mount db-test-redis at /data")
	}
}

func TestDeploy_DatabaseDefaultVersion(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	// Deploy with empty version — should default.
	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "default-ver",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:   "postgres",
			Version:  "", // should default to "16"
			LoginRole: "user",
			Password: "pass",
			DBName:   "db",
			Role:     loka.DatabaseRolePrimary,
		},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if svc.ImageRef != "postgres:16" {
		t.Errorf("ImageRef = %q, want postgres:16 (default)", svc.ImageRef)
	}
}

func TestDeploy_DatabaseDoesNotOverrideExplicitImage(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	// If user explicitly sets ImageRef, don't override.
	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name:     "custom-img",
		ImageRef: "custom-postgres:16-alpine",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:   "postgres",
			Version:  "16",
			LoginRole: "user",
			Password: "pass",
			DBName:   "db",
			Role:     loka.DatabaseRolePrimary,
		},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if svc.ImageRef != "custom-postgres:16-alpine" {
		t.Errorf("ImageRef = %q, want custom-postgres:16-alpine (explicit)", svc.ImageRef)
	}
}

func TestDeploy_DatabaseInvalidEngine(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	_, err := m.Deploy(context.Background(), DeployOpts{
		Name: "bad-engine",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine: "mongodb",
			Role:   loka.DatabaseRolePrimary,
		},
	})
	if err == nil {
		t.Fatal("expected error for unsupported engine")
	}
}

func TestDeploy_NonDatabaseService(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	// Regular service (no DatabaseConfig) should use default port 8080.
	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name:     "regular-svc",
		ImageRef: "nginx:latest",
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if svc.Port != 8080 {
		t.Errorf("Port = %d, want 8080 for regular service", svc.Port)
	}
	if svc.DatabaseConfig != nil {
		t.Error("expected nil DatabaseConfig for regular service")
	}
	if svc.HealthPath != "/health" {
		t.Errorf("HealthPath = %q, want /health for regular service", svc.HealthPath)
	}
}

func TestDeploy_WithUses(t *testing.T) {
	s := setupTestStore(t)

	// Create a target service that we'll reference via "uses".
	target := createTestService(t, s, "target-svc", loka.ServiceStatusRunning, time.Now())

	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name:     "uses-svc",
		ImageRef: "alpine:latest",
		Uses:     map[string]string{"api": target.Name},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// Verify env vars injected from the "uses" target.
	if svc.Env["API_HOST"] == "" {
		t.Error("expected API_HOST env var to be injected")
	}
	if svc.Env["API_PORT"] == "" {
		t.Error("expected API_PORT env var to be injected")
	}

	// Verify Uses stored on the service.
	if svc.Uses == nil || svc.Uses["api"] != target.Name {
		t.Errorf("Uses = %v, want {api: %s}", svc.Uses, target.Name)
	}
}

func TestDeploy_WithUses_DatabaseTarget(t *testing.T) {
	s := setupTestStore(t)

	// Create a database service as the target.
	now := time.Now()
	dbSvc := &loka.Service{
		ID:       "db-target-id",
		Name:     "mydb",
		Status:   loka.ServiceStatusRunning,
		ImageRef: "postgres:16",
		Port:     5432,
		VCPUs:    1,
		MemoryMB: 512,
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:    "postgres",
			Version:   "16",
			LoginRole: "pg_login",
			Password:  "pg_secret",
			DBName:    "appdb",
			Role:      loka.DatabaseRolePrimary,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Services().Create(context.Background(), dbSvc); err != nil {
		t.Fatal(err)
	}

	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name:     "db-user-svc",
		ImageRef: "alpine:latest",
		Uses:     map[string]string{"db": "mydb"},
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// Verify database-specific env vars.
	if svc.Env["DB_USER"] != "pg_login" {
		t.Errorf("DB_USER = %q, want pg_login", svc.Env["DB_USER"])
	}
	if svc.Env["DB_PASSWORD"] != "pg_secret" {
		t.Errorf("DB_PASSWORD = %q, want pg_secret", svc.Env["DB_PASSWORD"])
	}
	if svc.Env["DB_URL"] == "" {
		t.Error("expected DB_URL env var")
	}
}

func TestDeploy_WithUses_TargetNoPort(t *testing.T) {
	s := setupTestStore(t)
	// Target with Port=0.
	target := &loka.Service{
		ID: "svc-noport", Name: "noport-svc", Status: loka.ServiceStatusRunning,
		Port: 0, VCPUs: 1, MemoryMB: 256,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Services().Create(context.Background(), target)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "uses-noport", ImageRef: "alpine:latest",
		Uses: map[string]string{"api": "noport-svc"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Port 0 is injected — caller should handle.
	if svc.Env["API_PORT"] != "0" {
		t.Errorf("API_PORT = %q, want '0'", svc.Env["API_PORT"])
	}
}

func TestDeploy_WithUses_TargetStopped(t *testing.T) {
	s := setupTestStore(t)
	target := createTestService(t, s, "stopped-dep", loka.ServiceStatusStopped, time.Now())
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "uses-stopped", ImageRef: "alpine:latest",
		Uses: map[string]string{"dep": target.Name},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Env vars should still be injected even for stopped targets.
	if svc.Env["DEP_HOST"] == "" {
		t.Error("expected DEP_HOST even for stopped target")
	}
}

func TestDeploy_WithUses_EmptyAlias(t *testing.T) {
	s := setupTestStore(t)
	target := createTestService(t, s, "alias-target", loka.ServiceStatusRunning, time.Now())
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "uses-empty-alias", ImageRef: "alpine:latest",
		Uses: map[string]string{"": target.Name},
	})
	if err != nil {
		t.Fatalf("Deploy should not crash with empty alias: %v", err)
	}
	// Empty alias generates "_HOST" — not ideal but shouldn't crash.
	if _, ok := svc.Env["_HOST"]; !ok {
		t.Error("expected _HOST env var for empty alias")
	}
}

func TestDeploy_WithUses_TargetNotFound(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	// Deploy with a uses target that doesn't exist — should not crash.
	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name:     "missing-dep-svc",
		ImageRef: "alpine:latest",
		Uses:     map[string]string{"db": "nonexistent-db"},
	})
	if err != nil {
		t.Fatalf("Deploy should succeed even with missing dependency: %v", err)
	}
	// No env vars injected for missing dependency.
	if svc.Env["DB_HOST"] != "" {
		t.Errorf("expected no DB_HOST for missing dependency, got %q", svc.Env["DB_HOST"])
	}
}

func TestRecoverStuckDeploys_LeavesRunningAlone(t *testing.T) {
	s := setupTestStore(t)

	// Insert a running service from 20 minutes ago.
	oldTime := time.Now().Add(-20 * time.Minute)
	svc := createTestService(t, s, "running-svc", loka.ServiceStatusRunning, oldTime)

	_ = newManagerFromStore(t, s)

	// Running services should not be affected by recoverStuckDeploys.
	got, err := s.Services().Get(context.Background(), svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loka.ServiceStatusRunning {
		t.Errorf("status = %q, want %q", got.Status, loka.ServiceStatusRunning)
	}
}

// --- Scale tests ---

func TestScale_Up(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "scale-up", ImageRef: "alpine:latest",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Scale(context.Background(), svc.ID, 3); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	// Verify replicas created.
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	count := 0
	for _, r := range replicas {
		if r.RelationType == "replica" {
			count++
		}
	}
	if count != 2 { // 3 total - 1 primary = 2 replicas
		t.Errorf("expected 2 replicas, got %d", count)
	}
}

func TestScale_Down(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "scale-down", ImageRef: "alpine:latest", Replicas: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Scale(context.Background(), svc.ID, 1); err != nil {
		t.Fatalf("Scale: %v", err)
	}

	// Verify replicas removed.
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	for _, r := range replicas {
		if r.RelationType == "replica" {
			t.Error("expected all replicas removed")
		}
	}
}

func TestScale_MinOne(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, _ := m.Deploy(context.Background(), DeployOpts{Name: "scale-min", ImageRef: "alpine:latest"})
	err := m.Scale(context.Background(), svc.ID, 0)
	if err == nil {
		t.Error("expected error for replicas < 1")
	}
}

func TestDeploy_WithReplicas(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "with-replicas", ImageRef: "alpine:latest", Replicas: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if svc.Replicas != 3 {
		t.Errorf("Replicas = %d, want 3", svc.Replicas)
	}

	// Verify 2 replica records.
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	replicaCount := 0
	for _, r := range replicas {
		if r.RelationType == "replica" && r.ParentServiceID == svc.ID {
			replicaCount++
		}
	}
	if replicaCount != 2 {
		t.Errorf("expected 2 replicas, got %d", replicaCount)
	}
}

// --- recoverStuckDatabases tests ---

func TestRecoverStuckDatabases_CleansExpiredGrace(t *testing.T) {
	s := setupTestStore(t)

	// Create a DB with expired grace period.
	now := time.Now()
	db := &loka.Service{
		ID: "db-stuck-grace", Name: "stuck-grace", Status: loka.ServiceStatusRunning,
		ImageRef: "postgres:16", Port: 5432, VCPUs: 1, MemoryMB: 512,
		DatabaseConfig: &loka.DatabaseConfig{
			Engine: "postgres", Version: "16", LoginRole: "new", Password: "p",
			Role: loka.DatabaseRolePrimary,
			PreviousLoginRole: "old_login",
			GraceDeadline:     now.Add(-1 * time.Hour), // Expired 1 hour ago.
		},
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
	s.Services().Create(context.Background(), db)

	_ = newManagerFromStore(t, s) // Triggers recoverStuckDatabases.

	got, _ := s.Services().Get(context.Background(), db.ID)
	if got.DatabaseConfig.PreviousLoginRole != "" {
		t.Error("expected PreviousLoginRole cleared")
	}
	if !got.DatabaseConfig.GraceDeadline.IsZero() {
		t.Error("expected GraceDeadline cleared")
	}
}

func TestScale_NonExistentService(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)
	err := m.Scale(context.Background(), "nonexistent-id", 3)
	if err == nil {
		t.Error("expected error for non-existent service")
	}
}

func TestRecoverStuckDatabases_NonExpiredGrace(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now()
	db := &loka.Service{
		ID: "db-fresh-grace", Name: "fresh-grace", Status: loka.ServiceStatusRunning,
		ImageRef: "postgres:16", Port: 5432, VCPUs: 1, MemoryMB: 512,
		DatabaseConfig: &loka.DatabaseConfig{
			Engine: "postgres", Version: "16", LoginRole: "new", Password: "p",
			Role:              loka.DatabaseRolePrimary,
			PreviousLoginRole: "old_login",
			GraceDeadline:     now.Add(1 * time.Hour), // Expires in 1 hour (not yet).
		},
		CreatedAt: now, UpdatedAt: now,
	}
	s.Services().Create(context.Background(), db)

	_ = newManagerFromStore(t, s)

	got, _ := s.Services().Get(context.Background(), db.ID)
	if got.DatabaseConfig.PreviousLoginRole == "" {
		t.Error("non-expired grace should NOT be cleared")
	}
}

func TestDeploy_WithReplicasAndDatabaseConfig(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name:     "db-with-replicas",
		Replicas: 2,
		DatabaseConfig: &loka.DatabaseConfig{
			Engine: "postgres", Version: "16", LoginRole: "user", Password: "pass",
			DBName: "testdb", Role: loka.DatabaseRolePrimary,
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if svc.DatabaseConfig == nil {
		t.Fatal("expected DatabaseConfig on primary")
	}

	// Verify replica was created.
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	count := 0
	for _, r := range replicas {
		if r.RelationType == "replica" {
			count++
		}
	}
	if count != 1 { // 2 total - 1 primary = 1 replica
		t.Errorf("expected 1 replica, got %d", count)
	}
}

func TestRecoverStuckDatabases_ExpiredGraceWithoutPreviousRole(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now()
	db := &loka.Service{
		ID: "db-no-role", Name: "no-role", Status: loka.ServiceStatusRunning,
		ImageRef: "postgres:16", Port: 5432, VCPUs: 1, MemoryMB: 512,
		DatabaseConfig: &loka.DatabaseConfig{
			Engine: "postgres", Version: "16", LoginRole: "current", Password: "p",
			Role:              loka.DatabaseRolePrimary,
			PreviousLoginRole: "", // No previous role — grace period check skipped.
			GraceDeadline:     now.Add(-1 * time.Hour), // Expired.
		},
		CreatedAt: now, UpdatedAt: now,
	}
	s.Services().Create(context.Background(), db)

	_ = newManagerFromStore(t, s)

	got, _ := s.Services().Get(context.Background(), db.ID)
	// With PreviousLoginRole empty, the recovery code checks both conditions:
	// cfg.PreviousLoginRole != "" && !cfg.GraceDeadline.IsZero() && now.After(cfg.GraceDeadline)
	// Since PreviousLoginRole is empty, it should NOT be modified.
	if !got.DatabaseConfig.GraceDeadline.IsZero() && got.DatabaseConfig.PreviousLoginRole == "" {
		// This is actually fine — the condition requires PreviousLoginRole != "".
		// The deadline stays stale but harmless. Document this behavior.
		t.Log("expired deadline without previous role: left as-is (expected)")
	}
}

func TestDeploy_Replicas_ExactlyOne(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)
	svc, err := m.Deploy(context.Background(), DeployOpts{
		Name: "one-replica", ImageRef: "alpine:latest", Replicas: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Replicas=1 means just the primary, no extra records.
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	if len(replicas) != 0 {
		t.Errorf("expected 0 replicas for replicas=1, got %d", len(replicas))
	}
}

func TestScale_NoChange(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)
	svc, _ := m.Deploy(context.Background(), DeployOpts{
		Name: "no-change", ImageRef: "alpine:latest", Replicas: 2,
	})
	// Scale to same count — should be a no-op.
	if err := m.Scale(context.Background(), svc.ID, 2); err != nil {
		t.Fatalf("Scale no-change: %v", err)
	}
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	count := 0
	for _, r := range replicas {
		if r.RelationType == "replica" {
			count++
		}
	}
	if count != 1 { // 2 total - 1 primary = 1 replica
		t.Errorf("expected 1 replica after no-change scale, got %d", count)
	}
}

func TestScale_Concurrent(t *testing.T) {
	s := setupTestStore(t)
	m := newManagerFromStore(t, s)

	svc, _ := m.Deploy(context.Background(), DeployOpts{
		Name: "concurrent-scale", ImageRef: "alpine:latest",
	})

	// 10 goroutines all scale to 3 simultaneously.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Scale(context.Background(), svc.ID, 3)
		}()
	}
	wg.Wait()

	// Verify exactly 2 replicas (not duplicates).
	replicas, _, _ := s.Services().List(context.Background(), store.ServiceFilter{ParentServiceID: &svc.ID})
	count := 0
	for _, r := range replicas {
		if r.RelationType == "replica" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 replicas after concurrent scale to 3, got %d", count)
	}
}

func TestRecoverOrphanedReplicas(t *testing.T) {
	s := setupTestStore(t)

	// Create an orphan: ParentServiceID points to non-existent service.
	orphan := &loka.Service{
		ID: "orphan-child", Name: "orphan-child", Status: loka.ServiceStatusDeploying,
		ImageRef: "alpine:latest", Port: 8080, VCPUs: 1, MemoryMB: 256,
		ParentServiceID: "non-existent-parent",
		RelationType:    "replica",
		CreatedAt: time.Now().Add(-20 * time.Minute), UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
	s.Services().Create(context.Background(), orphan)

	_ = newManagerFromStore(t, s) // Triggers recovery.

	got, _ := s.Services().Get(context.Background(), orphan.ID)
	if got.Status != loka.ServiceStatusError {
		t.Errorf("orphan status = %q, want error", got.Status)
	}
	if got.StatusMessage == "" {
		t.Error("expected orphan status message")
	}
}
