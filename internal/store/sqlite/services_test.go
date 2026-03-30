package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// newTestService creates a fully populated loka.Service for testing.
func newTestService(name string, status loka.ServiceStatus, workerID string) *loka.Service {
	now := time.Now().Truncate(time.Second).UTC()
	return &loka.Service{
		ID:         uuid.New().String(),
		Name:       name,
		Status:     status,
		WorkerID:   workerID,
		ImageRef:   "nginx:latest",
		ImageID:    "sha256:abc123",
		RecipeName: "web",
		Command:    "nginx",
		Args:       []string{"-g", "daemon off;"},
		Env: map[string]string{
			"PORT":     "8080",
			"LOG_LEVEL": "info",
		},
		Workdir:        "/app",
		Port:           8080,
		VCPUs:          2,
		MemoryMB:       512,
		Routes:         []loka.ServiceRoute{{Domain: "api", Port: 8080, Protocol: "http"}},
		BundleKey:      "bundle-123",
		IdleTimeout:    300,
		HealthPath:     "/healthz",
		HealthInterval: 10,
		HealthTimeout:  5,
		HealthRetries:  3,
		Labels:         map[string]string{"env": "test"},
		Mounts:         []loka.Volume{{Path: "/data", Provider: "volume", Name: "data-vol"}},
		Autoscale: &loka.AutoscaleConfig{
			Min:                1,
			Max:                5,
			TargetConcurrency:  100,
			ScaleUpThreshold:   0.8,
			ScaleDownThreshold: 0.3,
			Cooldown:           60,
		},
		SnapshotID:    "snap-001",
		Ready:         true,
		StatusMessage: "all good",
		LastActivity:  now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestServiceCreate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("my-svc", loka.ServiceStatusRunning, "worker-1")
	err := s.Services().Create(ctx, svc)
	require.NoError(t, err)

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Equal(t, svc.ID, got.ID)
	require.Equal(t, "my-svc", got.Name)
	require.Equal(t, loka.ServiceStatusRunning, got.Status)
	require.Equal(t, "worker-1", got.WorkerID)
	require.Equal(t, "nginx:latest", got.ImageRef)
	require.Equal(t, 8080, got.Port)
	require.Equal(t, 2, got.VCPUs)
	require.Equal(t, 512, got.MemoryMB)
	require.True(t, got.Ready)
}

func TestServiceGet(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("get-svc", loka.ServiceStatusDeploying, "worker-2")
	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)

	// Verify all scalar fields.
	require.Equal(t, svc.Name, got.Name)
	require.Equal(t, svc.Status, got.Status)
	require.Equal(t, svc.WorkerID, got.WorkerID)
	require.Equal(t, svc.ImageRef, got.ImageRef)
	require.Equal(t, svc.ImageID, got.ImageID)
	require.Equal(t, svc.RecipeName, got.RecipeName)
	require.Equal(t, svc.Command, got.Command)
	require.Equal(t, svc.Workdir, got.Workdir)
	require.Equal(t, svc.Port, got.Port)
	require.Equal(t, svc.BundleKey, got.BundleKey)
	require.Equal(t, svc.IdleTimeout, got.IdleTimeout)
	require.Equal(t, svc.HealthPath, got.HealthPath)
	require.Equal(t, svc.HealthInterval, got.HealthInterval)
	require.Equal(t, svc.HealthTimeout, got.HealthTimeout)
	require.Equal(t, svc.HealthRetries, got.HealthRetries)
	require.Equal(t, svc.SnapshotID, got.SnapshotID)
	require.Equal(t, svc.Ready, got.Ready)
	require.Equal(t, svc.StatusMessage, got.StatusMessage)

	// Verify JSON fields.
	require.Equal(t, svc.Args, got.Args)
	require.Equal(t, svc.Env, got.Env)
	require.Equal(t, svc.Routes, got.Routes)
	require.Equal(t, svc.Labels, got.Labels)
	require.Equal(t, svc.Mounts, got.Mounts)
	require.Equal(t, svc.Autoscale, got.Autoscale)
}

func TestServiceGetNotFound(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	_, err := s.Services().Get(ctx, "nonexistent-id")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestServiceUpdate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("update-svc", loka.ServiceStatusDeploying, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc))

	// Modify fields.
	svc.Name = "updated-svc"
	svc.Status = loka.ServiceStatusRunning
	svc.Port = 9090
	svc.Ready = false
	svc.StatusMessage = "updated message"
	svc.Env["NEW_KEY"] = "new_value"
	svc.Args = append(svc.Args, "--verbose")

	require.NoError(t, s.Services().Update(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Equal(t, "updated-svc", got.Name)
	require.Equal(t, loka.ServiceStatusRunning, got.Status)
	require.Equal(t, 9090, got.Port)
	require.False(t, got.Ready)
	require.Equal(t, "updated message", got.StatusMessage)
	require.Equal(t, "new_value", got.Env["NEW_KEY"])
	require.Contains(t, got.Args, "--verbose")
}

func TestServiceDelete(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("delete-svc", loka.ServiceStatusRunning, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc))

	require.NoError(t, s.Services().Delete(ctx, svc.ID))

	_, err := s.Services().Get(ctx, svc.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestServiceList(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("svc-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("svc-2", loka.ServiceStatusStopped, "worker-2")
	svc3 := newTestService("svc-3", loka.ServiceStatusDeploying, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, svc3))

	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{})
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, svcs, 3)
}

func TestServiceListByStatus(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("running-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("running-2", loka.ServiceStatusRunning, "worker-2")
	svc3 := newTestService("stopped-1", loka.ServiceStatusStopped, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, svc3))

	running := loka.ServiceStatusRunning
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Status: &running})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, svcs, 2)
	for _, svc := range svcs {
		require.Equal(t, loka.ServiceStatusRunning, svc.Status)
	}
}

func TestServiceListByWorker(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("w1-svc-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("w1-svc-2", loka.ServiceStatusStopped, "worker-1")
	svc3 := newTestService("w2-svc-1", loka.ServiceStatusRunning, "worker-2")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, svc3))

	wid := "worker-1"
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{WorkerID: &wid})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, svcs, 2)
	for _, svc := range svcs {
		require.Equal(t, "worker-1", svc.WorkerID)
	}
}

func TestServiceListByName(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("target-svc", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("other-svc", loka.ServiceStatusRunning, "worker-1")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))

	name := "target-svc"
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Name: &name})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, svcs, 1)
	require.Equal(t, "target-svc", svcs[0].Name)
}

func TestServiceListPagination(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Create 5 services.
	for i := 0; i < 5; i++ {
		svc := newTestService("svc-"+uuid.New().String()[:4], loka.ServiceStatusRunning, "worker-1")
		// Stagger creation times to get deterministic ordering.
		svc.CreatedAt = time.Now().Add(time.Duration(i) * time.Second).Truncate(time.Second).UTC()
		require.NoError(t, s.Services().Create(ctx, svc))
	}

	// First page: limit 2, offset 0.
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, 5, total)
	require.Len(t, svcs, 2)

	// Second page: limit 2, offset 2.
	svcs2, total2, err := s.Services().List(ctx, store.ServiceFilter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, 5, total2)
	require.Len(t, svcs2, 2)

	// Pages should not overlap.
	require.NotEqual(t, svcs[0].ID, svcs2[0].ID)

	// Last page: limit 2, offset 4.
	svcs3, _, err := s.Services().List(ctx, store.ServiceFilter{Limit: 2, Offset: 4})
	require.NoError(t, err)
	require.Len(t, svcs3, 1)
}

func TestServiceListByWorkerMethod(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc1 := newTestService("w1-svc", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("w2-svc", loka.ServiceStatusRunning, "worker-2")
	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))

	svcs, err := s.Services().ListByWorker(ctx, "worker-1")
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	require.Equal(t, "worker-1", svcs[0].WorkerID)
}

func TestServiceDatabaseConfigRoundtrip(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("db-svc", loka.ServiceStatusRunning, "worker-1")
	svc.DatabaseConfig = &loka.DatabaseConfig{
		Engine:    "postgres",
		Version:   "16",
		LoginRole:  "pguser",
		Password:  "pgpass",
		DBName:    "mydb",
		Role:      loka.DatabaseRolePrimary,
		PrimaryID: "",
		Backup: &loka.BackupConfig{
			Enabled:   true,
			Schedule:  "0 */6 * * *",
			Retention: 7,
			WAL:       true,
		},
	}

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DatabaseConfig)
	require.Equal(t, "postgres", got.DatabaseConfig.Engine)
	require.Equal(t, "16", got.DatabaseConfig.Version)
	require.Equal(t, "pguser", got.DatabaseConfig.LoginRole)
	require.Equal(t, "pgpass", got.DatabaseConfig.Password)
	require.Equal(t, "mydb", got.DatabaseConfig.DBName)
	require.Equal(t, loka.DatabaseRolePrimary, got.DatabaseConfig.Role)
	require.NotNil(t, got.DatabaseConfig.Backup)
	require.True(t, got.DatabaseConfig.Backup.Enabled)
	require.Equal(t, "0 */6 * * *", got.DatabaseConfig.Backup.Schedule)
	require.Equal(t, 7, got.DatabaseConfig.Backup.Retention)
	require.True(t, got.DatabaseConfig.Backup.WAL)
}

func TestServiceDatabaseConfigNil(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("no-db-svc", loka.ServiceStatusRunning, "worker-1")
	// DatabaseConfig is nil by default.
	require.Nil(t, svc.DatabaseConfig)

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Nil(t, got.DatabaseConfig)
}

func TestServiceDatabaseConfigReplicaRoundtrip(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("db-replica", loka.ServiceStatusRunning, "worker-1")
	svc.DatabaseConfig = &loka.DatabaseConfig{
		Engine:    "mysql",
		Version:   "8.0",
		LoginRole:  "repl",
		Password:  "replpass",
		DBName:    "appdb",
		Role:      loka.DatabaseRoleReplica,
		PrimaryID: "primary-svc-id-123",
	}

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DatabaseConfig)
	require.Equal(t, loka.DatabaseRoleReplica, got.DatabaseConfig.Role)
	require.Equal(t, "primary-svc-id-123", got.DatabaseConfig.PrimaryID)
}

func TestServiceDatabaseConfigUpdate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("update-db-svc", loka.ServiceStatusRunning, "worker-1")
	svc.DatabaseConfig = &loka.DatabaseConfig{
		Engine:   "redis",
		Version:  "7",
		Password: "oldpass",
		Role:     loka.DatabaseRolePrimary,
	}
	require.NoError(t, s.Services().Create(ctx, svc))

	// Update the password.
	svc.DatabaseConfig.Password = "newpass"
	require.NoError(t, s.Services().Update(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DatabaseConfig)
	require.Equal(t, "newpass", got.DatabaseConfig.Password)
}

func TestServiceListByIsDatabase(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Create 2 regular services and 2 database services.
	svc1 := newTestService("regular-1", loka.ServiceStatusRunning, "worker-1")
	svc2 := newTestService("regular-2", loka.ServiceStatusRunning, "worker-1")
	db1 := newTestService("db-postgres", loka.ServiceStatusRunning, "worker-1")
	db1.DatabaseConfig = &loka.DatabaseConfig{Engine: "postgres", Version: "16", Role: loka.DatabaseRolePrimary}
	db2 := newTestService("db-redis", loka.ServiceStatusRunning, "worker-1")
	db2.DatabaseConfig = &loka.DatabaseConfig{Engine: "redis", Version: "7", Role: loka.DatabaseRolePrimary}

	require.NoError(t, s.Services().Create(ctx, svc1))
	require.NoError(t, s.Services().Create(ctx, svc2))
	require.NoError(t, s.Services().Create(ctx, db1))
	require.NoError(t, s.Services().Create(ctx, db2))

	// Filter: only databases.
	isDB := true
	dbs, total, err := s.Services().List(ctx, store.ServiceFilter{IsDatabase: &isDB})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, dbs, 2)
	for _, svc := range dbs {
		require.NotNil(t, svc.DatabaseConfig, "expected DatabaseConfig for %s", svc.Name)
	}

	// Filter: only non-databases.
	notDB := false
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{IsDatabase: &notDB})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, svcs, 2)
	for _, svc := range svcs {
		require.Nil(t, svc.DatabaseConfig, "expected nil DatabaseConfig for %s", svc.Name)
	}

	// No filter: returns all.
	all, total, err := s.Services().List(ctx, store.ServiceFilter{})
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Len(t, all, 4)
}

func TestServiceListByIsDatabaseInList(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Verify that the IsDatabase filter works correctly with the List method
	// when combined with other filters.
	db := newTestService("db-filtered", loka.ServiceStatusStopped, "worker-1")
	db.DatabaseConfig = &loka.DatabaseConfig{Engine: "postgres", Version: "16", Role: loka.DatabaseRolePrimary}
	regular := newTestService("regular-filtered", loka.ServiceStatusStopped, "worker-1")

	require.NoError(t, s.Services().Create(ctx, db))
	require.NoError(t, s.Services().Create(ctx, regular))

	// Filter by status AND isDatabase.
	stopped := loka.ServiceStatusStopped
	isDB := true
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{Status: &stopped, IsDatabase: &isDB})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, svcs, 1)
	require.Equal(t, "db-filtered", svcs[0].Name)
}

func TestServiceUsesRoundtrip(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("uses-svc", loka.ServiceStatusRunning, "worker-1")
	svc.Uses = map[string]string{
		"db":    "mydb",
		"cache": "shared-redis",
	}

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Uses)
	require.Equal(t, "mydb", got.Uses["db"])
	require.Equal(t, "shared-redis", got.Uses["cache"])
}

func TestServiceUsesNil(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("no-uses-svc", loka.ServiceStatusRunning, "worker-1")
	// Uses is nil by default.

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Empty(t, got.Uses)
}

func TestServiceListByPrimaryID(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	primary := newTestService("primary-svc", loka.ServiceStatusRunning, "worker-1")
	primary.DatabaseConfig = &loka.DatabaseConfig{Engine: "postgres", Version: "16", Role: loka.DatabaseRolePrimary}
	require.NoError(t, s.Services().Create(ctx, primary))

	replica1 := newTestService("replica-1", loka.ServiceStatusRunning, "worker-1")
	replica1.DatabaseConfig = &loka.DatabaseConfig{
		Engine: "postgres", Version: "16", Role: loka.DatabaseRoleReplica, PrimaryID: primary.ID,
	}
	require.NoError(t, s.Services().Create(ctx, replica1))

	replica2 := newTestService("replica-2", loka.ServiceStatusRunning, "worker-1")
	replica2.DatabaseConfig = &loka.DatabaseConfig{
		Engine: "postgres", Version: "16", Role: loka.DatabaseRoleReplica, PrimaryID: primary.ID,
	}
	require.NoError(t, s.Services().Create(ctx, replica2))

	// Unrelated replica (different primary).
	other := newTestService("other-replica", loka.ServiceStatusRunning, "worker-1")
	other.DatabaseConfig = &loka.DatabaseConfig{
		Engine: "postgres", Version: "16", Role: loka.DatabaseRoleReplica, PrimaryID: "other-primary-id",
	}
	require.NoError(t, s.Services().Create(ctx, other))

	// Filter by PrimaryID.
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{PrimaryID: &primary.ID})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, svcs, 2)
	for _, svc := range svcs {
		require.Equal(t, primary.ID, svc.DatabaseConfig.PrimaryID)
	}
}

func TestServiceListByPrimaryID_WithWildcards(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Create a service with a normal PrimaryID.
	svc := newTestService("wildcard-test", loka.ServiceStatusRunning, "worker-1")
	svc.DatabaseConfig = &loka.DatabaseConfig{
		Engine: "postgres", Version: "16", Role: loka.DatabaseRoleReplica, PrimaryID: "real-primary-id",
	}
	require.NoError(t, s.Services().Create(ctx, svc))

	// Filter with "%" wildcard — should NOT match everything.
	wildcard := "%"
	svcs, total, err := s.Services().List(ctx, store.ServiceFilter{PrimaryID: &wildcard})
	require.NoError(t, err)
	// The LIKE query embeds "%" literally inside the JSON pattern, so
	// searching for PrimaryID="%" should not match "real-primary-id".
	require.Equal(t, 0, total, "wildcard %% in PrimaryID should not match real IDs")
	require.Len(t, svcs, 0)
}

func TestUnmarshalDatabaseConfig_InvalidJSON(t *testing.T) {
	// Directly test the unmarshal helper.
	var svc loka.Service
	unmarshalDatabaseConfig("{broken json", &svc)
	require.Nil(t, svc.DatabaseConfig, "malformed JSON should result in nil DatabaseConfig")
}

func TestUnmarshalDatabaseConfig_EmptyString(t *testing.T) {
	var svc loka.Service
	unmarshalDatabaseConfig("", &svc)
	require.Nil(t, svc.DatabaseConfig, "empty string should result in nil DatabaseConfig")
}

func TestServiceReplicaFieldsRoundtrip(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("replica-fields", loka.ServiceStatusRunning, "worker-1")
	svc.ParentServiceID = "parent-123"
	svc.Replicas = 3
	svc.RelationType = "replica"

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)
	require.Equal(t, "parent-123", got.ParentServiceID)
	require.Equal(t, 3, got.Replicas)
	require.Equal(t, "replica", got.RelationType)
}

func TestServiceJSONFields(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	svc := newTestService("json-svc", loka.ServiceStatusRunning, "worker-1")

	// Set up complex JSON fields.
	svc.Routes = []loka.ServiceRoute{
		{Domain: "api", Port: 8080, Protocol: "http"},
		{CustomDomain: "app.example.com", Port: 443, Protocol: "http"},
		{Domain: "grpc", Port: 9090, Protocol: "grpc"},
	}
	svc.Mounts = []loka.Volume{
		{Path: "/data", Provider: "volume", Name: "data-vol", Access: "readwrite"},
		{Path: "/cache", Provider: "s3", Bucket: "my-bucket", Region: "us-east-1", Credentials: "${secret.aws}", Access: "readonly"},
		{Path: "/backup", Provider: "gcs", Bucket: "backup-bucket", Region: "us-central1"},
	}
	svc.Env = map[string]string{
		"PORT":          "8080",
		"LOG_LEVEL":     "debug",
		"DATABASE_URL":  "postgres://localhost/db",
		"NESTED_EQUALS": "key=value",
	}
	svc.Args = []string{"-c", "config.yaml", "--verbose", "--port=8080"}
	svc.Autoscale = &loka.AutoscaleConfig{
		Min:                2,
		Max:                10,
		TargetConcurrency:  200,
		ScaleUpThreshold:   0.75,
		ScaleDownThreshold: 0.25,
		Cooldown:           120,
	}
	svc.Labels = map[string]string{
		"env":     "production",
		"team":    "platform",
		"version": "v2.1.0",
	}

	require.NoError(t, s.Services().Create(ctx, svc))

	got, err := s.Services().Get(ctx, svc.ID)
	require.NoError(t, err)

	// Routes round-trip.
	require.Len(t, got.Routes, 3)
	require.Equal(t, "api", got.Routes[0].Domain)
	require.Equal(t, "app.example.com", got.Routes[1].CustomDomain)
	require.Equal(t, 443, got.Routes[1].Port)
	require.Equal(t, "grpc", got.Routes[2].Protocol)

	// Mounts round-trip.
	require.Len(t, got.Mounts, 3)
	require.Equal(t, "volume", got.Mounts[0].Provider)
	require.Equal(t, "readwrite", got.Mounts[0].Access)
	require.Equal(t, "s3", got.Mounts[1].Provider)
	require.Equal(t, "my-bucket", got.Mounts[1].Bucket)
	require.Equal(t, "${secret.aws}", got.Mounts[1].Credentials)
	require.Equal(t, "gcs", got.Mounts[2].Provider)

	// Env round-trip.
	require.Len(t, got.Env, 4)
	require.Equal(t, "debug", got.Env["LOG_LEVEL"])
	require.Equal(t, "key=value", got.Env["NESTED_EQUALS"])

	// Args round-trip.
	require.Equal(t, []string{"-c", "config.yaml", "--verbose", "--port=8080"}, got.Args)

	// Autoscale round-trip.
	require.NotNil(t, got.Autoscale)
	require.Equal(t, 2, got.Autoscale.Min)
	require.Equal(t, 10, got.Autoscale.Max)
	require.Equal(t, 200, got.Autoscale.TargetConcurrency)
	require.InDelta(t, 0.75, got.Autoscale.ScaleUpThreshold, 0.001)
	require.InDelta(t, 0.25, got.Autoscale.ScaleDownThreshold, 0.001)
	require.Equal(t, 120, got.Autoscale.Cooldown)

	// Labels round-trip.
	require.Len(t, got.Labels, 3)
	require.Equal(t, "production", got.Labels["env"])
	require.Equal(t, "platform", got.Labels["team"])
}
