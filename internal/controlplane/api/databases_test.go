package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/database"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/objstore/local"
)

// setupDatabaseTestServer creates a test server with a real service.Manager
// and a registered worker so database deploys can be scheduled.
func setupDatabaseTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	svcMgr := service.NewManager(ts.store, ts.registry, ts.sched, ts.imgMgr, nil, nil, ts.server.logger)
	t.Cleanup(func() { svcMgr.Close() })
	ts.server.serviceManager = svcMgr
	return ts
}

// createTestDatabase inserts a database service directly into the store.
func createTestDatabase(t *testing.T, ts *testServer, name, engine string, role loka.DatabaseRole, primaryID string) *loka.Service {
	t.Helper()
	now := time.Now()
	svc := &loka.Service{
		ID:       "db-" + name,
		Name:     name,
		Status:   loka.ServiceStatusRunning,
		ImageRef: engine + ":latest",
		Port:     5432,
		VCPUs:    1,
		MemoryMB: 512,
		Env:      map[string]string{},
		Labels:   map[string]string{},
		Routes:   []loka.ServiceRoute{},
		Ready:    true,
		GuestIP:  "10.0.0.100",
		DatabaseConfig: &loka.DatabaseConfig{
			Engine:    engine,
			Version:   "16",
			LoginRole:  "user",
			Password:  "pass123",
			DBName:    name,
			Role:      role,
			PrimaryID: primaryID,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := ts.store.Services().Create(context.Background(), svc); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	return svc
}

func TestCreateDatabase(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	payload := map[string]any{
		"engine": "postgres",
		"name":   "testdb",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases", payload, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID             string `json:"ID"`
		Name           string `json:"Name"`
		DatabaseConfig *struct {
			Engine   string `json:"engine"`
			Password string `json:"password"`
		} `json:"DatabaseConfig"`
	}
	decodeBody(t, rec, &resp)
	if resp.Name != "testdb" {
		t.Errorf("Name = %q, want testdb", resp.Name)
	}
	if resp.DatabaseConfig == nil {
		t.Fatal("expected DatabaseConfig to be set")
	}
	if resp.DatabaseConfig.Engine != "postgres" {
		t.Errorf("Engine = %q, want postgres", resp.DatabaseConfig.Engine)
	}
	// Password is redacted in response.
	if resp.DatabaseConfig.Password != "********" {
		t.Errorf("Password = %q, want ******** (redacted)", resp.DatabaseConfig.Password)
	}
}

func TestCreateDatabase_InvalidEngine(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	payload := map[string]any{
		"engine": "mongodb",
		"name":   "baddb",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases", payload, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateDatabase_MissingEngine(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	payload := map[string]any{"name": "noengine"}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases", payload, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateDatabase_WithExplicitPassword(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	payload := map[string]any{
		"engine":   "mysql",
		"name":     "mysql-explicit",
		"password": "my-secret-pass",
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases", payload, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	// Password is redacted in create response; verify via credentials endpoint.
	var resp struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}
	decodeBody(t, rec, &resp)

	credRec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/"+resp.ID+"/credentials", nil, nil)
	if credRec.Code != http.StatusOK {
		t.Fatalf("credentials: expected 200, got %d", credRec.Code)
	}
	var creds struct {
		Password string `json:"password"`
	}
	decodeBody(t, credRec, &creds)
	if creds.Password != "my-secret-pass" {
		t.Errorf("Password = %q, want my-secret-pass", creds.Password)
	}
}

func TestListDatabases(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	// Create a database and a regular service.
	createTestDatabase(t, ts, "mydb", "postgres", loka.DatabaseRolePrimary, "")
	createTestService(t, ts, "regular-svc", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Databases []*loka.Service `json:"databases"`
		Total     int             `json:"total"`
	}
	decodeBody(t, rec, &resp)

	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1 (only databases)", resp.Total)
	}
	if len(resp.Databases) != 1 {
		t.Fatalf("expected 1 database, got %d", len(resp.Databases))
	}
	if resp.Databases[0].Name != "mydb" {
		t.Errorf("Name = %q, want mydb", resp.Databases[0].Name)
	}
}

func TestListServices_ExcludesDatabases(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "hidden-db", "postgres", loka.DatabaseRolePrimary, "")
	createTestService(t, ts, "visible-svc", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Services []*loka.Service `json:"services"`
		Total    int             `json:"total"`
	}
	decodeBody(t, rec, &resp)

	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1 (databases hidden)", resp.Total)
	}
	for _, svc := range resp.Services {
		if svc.DatabaseConfig != nil {
			t.Errorf("service %q has DatabaseConfig, should be hidden", svc.Name)
		}
	}
}

func TestListServices_TypeAll(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "db-all", "postgres", loka.DatabaseRolePrimary, "")
	createTestService(t, ts, "svc-all", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services?type=all", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Services []*loka.Service `json:"services"`
		Total    int             `json:"total"`
	}
	decodeBody(t, rec, &resp)

	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 (type=all shows everything)", resp.Total)
	}
}

func TestGetDatabase(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	db := createTestDatabase(t, ts, "getdb", "postgres", loka.DatabaseRolePrimary, "")

	// Get by ID.
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/"+db.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Get by name.
	rec = ts.doRequest(t, http.MethodGet, "/api/v1/databases/getdb", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for name lookup, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetDatabase_NotFound(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/nonexistent", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetDatabase_RegularServiceNotFound(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	// A regular service should not be found via the database endpoint.
	createTestService(t, ts, "not-a-db", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/not-a-db", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for regular service, got %d", rec.Code)
	}
}

func TestGetDatabaseCredentials(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "creddb", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/creddb/credentials", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var creds struct {
		Engine   string `json:"engine"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		LoginRole string `json:"login_role"`
		Password string `json:"password"`
		DBName   string `json:"db_name"`
		URL      string `json:"url"`
	}
	decodeBody(t, rec, &creds)

	if creds.Engine != "postgres" {
		t.Errorf("Engine = %q, want postgres", creds.Engine)
	}
	// GuestIP is a runtime field (not persisted), so the fallback hostname is used.
	if creds.Host != "creddb.loka.internal" {
		t.Errorf("Host = %q, want creddb.loka.internal (fallback)", creds.Host)
	}
	if creds.Port != 5432 {
		t.Errorf("Port = %d, want 5432", creds.Port)
	}
	if creds.LoginRole != "user" {
		t.Errorf("LoginRole = %q, want user", creds.LoginRole)
	}
	if creds.Password != "pass123" {
		t.Errorf("Password = %q, want pass123", creds.Password)
	}
	if creds.URL == "" {
		t.Error("expected non-empty URL")
	}
}

func TestRotateDatabaseCredentials(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "rotatedb", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/rotatedb/credentials/rotate", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		LoginRole         string `json:"login_role"`
		Password          string `json:"password"`
		URL               string `json:"url"`
		PreviousLoginRole string `json:"previous_login_role"`
		GracePeriod       string `json:"grace_period"`
		GraceDeadline     string `json:"grace_deadline"`
	}
	decodeBody(t, rec, &resp)

	if resp.LoginRole == "" {
		t.Error("expected new login_role")
	}
	if resp.LoginRole == "user" {
		t.Error("login_role should differ from original")
	}
	if resp.Password == "" {
		t.Error("expected new password")
	}
	if resp.URL == "" {
		t.Error("expected non-empty URL")
	}
	if resp.PreviousLoginRole != "user" {
		t.Errorf("previous_login_role = %q, want 'user'", resp.PreviousLoginRole)
	}
	if resp.GracePeriod == "" {
		t.Error("expected grace_period in response")
	}
	if resp.GraceDeadline == "" {
		t.Error("expected grace_deadline in response")
	}
}

func TestSetDatabaseCredentials(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "setdb", "mysql", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPut, "/api/v1/databases/setdb/credentials",
		map[string]any{"password": "new-password"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Password string `json:"password"`
	}
	decodeBody(t, rec, &resp)
	if resp.Password != "new-password" {
		t.Errorf("Password = %q, want new-password", resp.Password)
	}
}

func TestSetDatabaseCredentials_EmptyPassword(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "emptypassdb", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPut, "/api/v1/databases/emptypassdb/credentials",
		map[string]any{"password": ""}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty password, got %d", rec.Code)
	}
}

func TestStopDatabase(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	createTestDatabase(t, ts, "stopdb", "redis", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/stopdb/stop", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDestroyDatabase_CascadesReplicas(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	primary := createTestDatabase(t, ts, "primary-db", "postgres", loka.DatabaseRolePrimary, "")
	createTestDatabase(t, ts, "primary-db-replica-1", "postgres", loka.DatabaseRoleReplica, primary.ID)
	createTestDatabase(t, ts, "primary-db-replica-2", "postgres", loka.DatabaseRoleReplica, primary.ID)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/databases/"+primary.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify replicas are also destroyed.
	_, err := ts.store.Services().Get(context.Background(), "db-primary-db-replica-1")
	if err == nil {
		t.Error("expected replica-1 to be destroyed")
	}
	_, err = ts.store.Services().Get(context.Background(), "db-primary-db-replica-2")
	if err == nil {
		t.Error("expected replica-2 to be destroyed")
	}
}

func TestListDatabaseReplicas(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	primary := createTestDatabase(t, ts, "repl-primary", "postgres", loka.DatabaseRolePrimary, "")
	createTestDatabase(t, ts, "repl-r1", "postgres", loka.DatabaseRoleReplica, primary.ID)
	createTestDatabase(t, ts, "repl-r2", "postgres", loka.DatabaseRoleReplica, primary.ID)
	// Another DB's replica (should NOT appear).
	createTestDatabase(t, ts, "other-r1", "postgres", loka.DatabaseRoleReplica, "some-other-id")

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/"+primary.ID+"/replicas", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Replicas []*loka.Service `json:"replicas"`
	}
	decodeBody(t, rec, &resp)

	if len(resp.Replicas) != 2 {
		t.Fatalf("expected 2 replicas, got %d", len(resp.Replicas))
	}
	for _, r := range resp.Replicas {
		if r.DatabaseConfig.PrimaryID != primary.ID {
			t.Errorf("replica %s has PrimaryID %q, want %q", r.Name, r.DatabaseConfig.PrimaryID, primary.ID)
		}
	}
}

func TestAddDatabaseReplica(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	primary := createTestDatabase(t, ts, "add-repl-primary", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+primary.ID+"/replicas",
		map[string]any{"count": 1}, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Replicas []*loka.Service `json:"replicas"`
	}
	decodeBody(t, rec, &resp)
	if len(resp.Replicas) != 1 {
		t.Fatalf("expected 1 new replica, got %d", len(resp.Replicas))
	}
}

func TestAddDatabaseReplica_NotPrimary(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	primary := createTestDatabase(t, ts, "np-primary", "postgres", loka.DatabaseRolePrimary, "")
	replica := createTestDatabase(t, ts, "np-replica", "postgres", loka.DatabaseRoleReplica, primary.ID)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+replica.ID+"/replicas",
		map[string]any{"count": 1}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (can't add replica to replica), got %d", rec.Code)
	}
}

func TestRemoveDatabaseReplica(t *testing.T) {
	ts := setupDatabaseTestServer(t)

	primary := createTestDatabase(t, ts, "rm-primary", "postgres", loka.DatabaseRolePrimary, "")
	replica := createTestDatabase(t, ts, "rm-replica", "postgres", loka.DatabaseRoleReplica, primary.ID)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/databases/"+primary.ID+"/replicas/"+replica.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify replica was deleted.
	_, err := ts.store.Services().Get(context.Background(), replica.ID)
	if err == nil {
		t.Error("expected replica to be destroyed")
	}
}

// --- Backup/Upgrade/Rollback Tests ---

// setupDatabaseTestServerWithBackup creates a test server with backup manager.
func setupDatabaseTestServerWithBackup(t *testing.T) *testServer {
	t.Helper()
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	tmpDir := t.TempDir()
	objStore, err := local.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	svcMgr := service.NewManager(ts.store, ts.registry, ts.sched, ts.imgMgr, nil, nil, ts.server.logger)
	t.Cleanup(func() { svcMgr.Close() })
	ts.server.serviceManager = svcMgr

	backupMgr := database.NewBackupManager(ts.store, objStore, ts.server.logger)
	t.Cleanup(func() { backupMgr.Close() })
	ts.server.backupManager = backupMgr

	return ts
}

func TestCreateDatabaseBackup(t *testing.T) {
	ts := setupDatabaseTestServerWithBackup(t)

	db := createTestDatabase(t, ts, "backup-db", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/backups", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BackupID string `json:"backup_id"`
	}
	decodeBody(t, rec, &resp)
	if resp.BackupID == "" {
		t.Error("expected non-empty backup_id")
	}
}

func TestCreateDatabaseBackup_NoObjStore(t *testing.T) {
	ts := setupDatabaseTestServer(t) // no backup manager
	db := createTestDatabase(t, ts, "no-backup", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/backups", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestListDatabaseBackups(t *testing.T) {
	ts := setupDatabaseTestServerWithBackup(t)
	db := createTestDatabase(t, ts, "list-bk", "postgres", loka.DatabaseRolePrimary, "")

	// Create 2 backups.
	ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/backups", nil, nil)
	ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/backups", nil, nil)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases/"+db.ID+"/backups", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Backups []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"backups"`
	}
	decodeBody(t, rec, &resp)
	if len(resp.Backups) != 2 {
		t.Errorf("expected 2 backups, got %d", len(resp.Backups))
	}
}

func TestRestoreDatabase(t *testing.T) {
	ts := setupDatabaseTestServerWithBackup(t)
	db := createTestDatabase(t, ts, "restore-db", "postgres", loka.DatabaseRolePrimary, "")

	// Create a backup first.
	bkRec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/backups", nil, nil)
	var bk struct {
		BackupID string `json:"backup_id"`
	}
	decodeBody(t, bkRec, &bk)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/restore",
		map[string]any{"backup_id": bk.BackupID}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRestoreDatabase_InvalidBackupID(t *testing.T) {
	ts := setupDatabaseTestServerWithBackup(t)
	db := createTestDatabase(t, ts, "bad-restore", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/restore",
		map[string]any{"backup_id": "nonexistent"}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid backup_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpgradeDatabase(t *testing.T) {
	ts := setupDatabaseTestServerWithBackup(t)
	db := createTestDatabase(t, ts, "upgrade-db", "mysql", loka.DatabaseRolePrimary, "")
	db.DatabaseConfig.Version = "5.7"
	db.ImageRef = "mysql:5.7"
	ts.store.Services().Update(context.Background(), db)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/upgrade",
		map[string]any{"target_version": "8.0"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status          string `json:"status"`
		PreviousVersion string `json:"previous_version"`
		TargetVersion   string `json:"target_version"`
	}
	decodeBody(t, rec, &resp)
	if resp.PreviousVersion != "5.7" {
		t.Errorf("previous_version = %q, want 5.7", resp.PreviousVersion)
	}
	if resp.TargetVersion != "8.0" {
		t.Errorf("target_version = %q, want 8.0", resp.TargetVersion)
	}
}

func TestUpgradeDatabase_SameVersion(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	db := createTestDatabase(t, ts, "same-ver", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/upgrade",
		map[string]any{"target_version": "16"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for same version, got %d", rec.Code)
	}
}

func TestUpgradeDatabase_ReplicaRejected(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	primary := createTestDatabase(t, ts, "upg-primary", "postgres", loka.DatabaseRolePrimary, "")
	replica := createTestDatabase(t, ts, "upg-replica", "postgres", loka.DatabaseRoleReplica, primary.ID)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+replica.ID+"/upgrade",
		map[string]any{"target_version": "15"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for replica upgrade, got %d", rec.Code)
	}
}

func TestRollbackDatabaseUpgrade(t *testing.T) {
	ts := setupDatabaseTestServerWithBackup(t)
	db := createTestDatabase(t, ts, "rollback-db", "mysql", loka.DatabaseRolePrimary, "")
	db.DatabaseConfig.Version = "5.7"
	db.ImageRef = "mysql:5.7"
	ts.store.Services().Update(context.Background(), db)

	// Upgrade first.
	ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/upgrade",
		map[string]any{"target_version": "8.0"}, nil)

	// Rollback.
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/upgrade/rollback", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RestoredVersion string `json:"restored_version"`
	}
	decodeBody(t, rec, &resp)
	if resp.RestoredVersion != "5.7" {
		t.Errorf("restored_version = %q, want 5.7", resp.RestoredVersion)
	}
}

func TestRollbackDatabaseUpgrade_NoPrevious(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	db := createTestDatabase(t, ts, "no-rollback", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/upgrade/rollback", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no previous version), got %d", rec.Code)
	}
}

// --- Edge case tests ---

func TestListDatabases_OffsetBeyondTotal(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	createTestDatabase(t, ts, "offset-db", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/databases?offset=999999", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Databases []*loka.Service `json:"databases"`
		Total     int             `json:"total"`
	}
	decodeBody(t, rec, &resp)
	if len(resp.Databases) != 0 {
		t.Errorf("expected 0 results with high offset, got %d", len(resp.Databases))
	}
}

func TestForceStopDatabase_AlreadyStopped(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	db := createTestDatabase(t, ts, "already-stopped", "postgres", loka.DatabaseRolePrimary, "")
	// Stop it first.
	ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/stop", nil, nil)
	// Force-stop again — should be idempotent.
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/force-stop", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("force-stop on stopped DB: expected 200, got %d", rec.Code)
	}
}

func TestUpgradeDatabase_EmptyTargetVersion(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	db := createTestDatabase(t, ts, "empty-ver-upg", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/databases/"+db.ID+"/upgrade",
		map[string]any{"target_version": ""}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty target_version, got %d", rec.Code)
	}
}

func TestSetDatabaseCredentials_ShortPassword(t *testing.T) {
	ts := setupDatabaseTestServer(t)
	db := createTestDatabase(t, ts, "short-pw", "postgres", loka.DatabaseRolePrimary, "")

	rec := ts.doRequest(t, http.MethodPut, "/api/v1/databases/"+db.ID+"/credentials",
		map[string]any{"password": "short"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short password, got %d", rec.Code)
	}
}
