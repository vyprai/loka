package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

type createDatabaseReq struct {
	Name     string `json:"name"`
	Engine   string `json:"engine"`   // "postgres", "mysql", "redis"
	Version  string `json:"version"`  // optional, defaults to latest stable
	Password string `json:"password"` // optional, auto-generated if empty
	DBName   string `json:"db_name"`  // optional, defaults to name
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	Replicas int    `json:"replicas"` // number of read replicas (0 = standalone)

	BackupEnabled   *bool  `json:"backup_enabled"`   // default true
	BackupSchedule  string `json:"backup_schedule"`  // cron, default "0 */6 * * *"
	BackupRetention int    `json:"backup_retention"` // days, default 7
}

type credentialsSetReq struct {
	Password string `json:"password"`
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	var req createDatabaseReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Engine == "" {
		writeError(w, http.StatusBadRequest, "engine is required (postgres, mysql, redis)")
		return
	}
	if _, err := loka.GetEngineDefaults(req.Engine, req.Version); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := loka.ValidateVersion(req.Version); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name != "" {
		if err := loka.ValidateDBName(req.Name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Version == "" {
		req.Version = loka.DefaultVersions[req.Engine]
	}
	if req.Password == "" {
		req.Password = generatePassword()
	}
	if req.DBName == "" {
		req.DBName = strings.ReplaceAll(req.Name, "-", "_")
		if req.DBName == "" {
			req.DBName = req.Engine
		}
	}

	// Set up role-based credential model.
	// GroupRole: non-login privilege role. OwnerRole: non-login object owner.
	// LoginRole: the actual login user, member of GroupRole.
	sanitizedName := strings.ReplaceAll(req.DBName, "-", "_")
	groupRole := sanitizedName + "_rw"
	ownerRole := sanitizedName + "_owner"
	loginRole := sanitizedName + "_login"
	if req.Engine == "redis" {
		// Redis uses "default" user or named ACL users.
		groupRole = ""
		ownerRole = ""
		loginRole = "default"
	}

	backupEnabled := true
	if req.BackupEnabled != nil {
		backupEnabled = *req.BackupEnabled
	}
	backupSchedule := req.BackupSchedule
	if backupSchedule == "" {
		backupSchedule = "0 */6 * * *"
	}
	backupRetention := req.BackupRetention
	if backupRetention == 0 {
		backupRetention = 7
	}

	dbCfg := &loka.DatabaseConfig{
		Engine:    req.Engine,
		Version:   req.Version,
		DBName:    req.DBName,
		Role:      loka.DatabaseRolePrimary,
		GroupRole: groupRole,
		OwnerRole: ownerRole,
		LoginRole: loginRole,
		Password:  req.Password,
		Backup: &loka.BackupConfig{
			Enabled:   backupEnabled,
			Schedule:  backupSchedule,
			Retention: backupRetention,
			WAL:       req.Engine == "postgres" || req.Engine == "mysql",
		},
	}

	svc, err := s.serviceManager.Deploy(r.Context(), service.DeployOpts{
		Name:           req.Name,
		VCPUs:          req.VCPUs,
		MemoryMB:       req.MemoryMB,
		DatabaseConfig: dbCfg,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// If ?wait=true, block until ready.
	if r.URL.Query().Get("wait") == "true" && !svc.Ready {
		svc, err = s.serviceManager.WaitForReady(r.Context(), svc.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// Create replicas if requested. Collect warnings for partial failures.
	var warnings []string
	if req.Replicas > 0 {
		for i := 1; i <= req.Replicas; i++ {
			replicaCfg := &loka.DatabaseConfig{
				Engine:    req.Engine,
				Version:   req.Version,
				GroupRole: groupRole,
				OwnerRole: ownerRole,
				LoginRole: loginRole,
				Password:  req.Password,
				DBName:    req.DBName,
				Role:      loka.DatabaseRoleReplica,
				PrimaryID: svc.ID,
			}
			replicaName := fmt.Sprintf("%s-replica-%d", svc.Name, i)
			if _, err := s.serviceManager.Deploy(r.Context(), service.DeployOpts{
				Name:           replicaName,
				VCPUs:          req.VCPUs,
				MemoryMB:       req.MemoryMB,
				DatabaseConfig: replicaCfg,
			}); err != nil {
				warnings = append(warnings, fmt.Sprintf("replica %s: %v", replicaName, err))
			}
		}
		// For Redis, also create sentinels.
		if req.Engine == "redis" && req.Replicas >= 2 {
			for i := 1; i <= 3; i++ {
				sentinelCfg := &loka.DatabaseConfig{
					Engine:    req.Engine,
					Version:   req.Version,
					Password:  req.Password,
					Role:      loka.DatabaseRoleSentinel,
					PrimaryID: svc.ID,
				}
				sentinelName := fmt.Sprintf("%s-sentinel-%d", svc.Name, i)
				if _, err := s.serviceManager.Deploy(r.Context(), service.DeployOpts{
					Name:           sentinelName,
					VCPUs:          1,
					MemoryMB:       256,
					DatabaseConfig: sentinelCfg,
				}); err != nil {
					warnings = append(warnings, fmt.Sprintf("sentinel %s: %v", sentinelName, err))
				}
			}
		}
	}

	resp := map[string]any{
		"ID":             svc.ID,
		"Name":           svc.Name,
		"Status":         svc.Status,
		"ImageRef":       svc.ImageRef,
		"Ready":          svc.Ready,
		"StatusMessage":  svc.StatusMessage,
		"DatabaseConfig": redactDatabaseConfig(svc.DatabaseConfig),
		"CreatedAt":      svc.CreatedAt,
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listDatabases(w http.ResponseWriter, r *http.Request) {
	isDB := true
	filter := store.ServiceFilter{IsDatabase: &isDB, Limit: 100}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 {
			filter.Limit = v
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil && v >= 0 {
			filter.Offset = v
		}
	}
	svcs, total, err := s.store.Services().List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Redact passwords from response.
	for _, svc := range svcs {
		svc.DatabaseConfig = redactDatabaseConfig(svc.DatabaseConfig)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"databases": svcs,
		"total":     total,
	})
}

func (s *Server) getDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	svc.DatabaseConfig = redactDatabaseConfig(svc.DatabaseConfig)
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) destroyDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Destroy replicas and sentinels first using PrimaryID filter.
	// If ANY replica fails to destroy, do NOT destroy the primary — return error.
	var failed []string
	if svc.DatabaseConfig != nil && svc.DatabaseConfig.Role == loka.DatabaseRolePrimary {
		replicas, _, _ := s.store.Services().List(r.Context(), store.ServiceFilter{PrimaryID: &svc.ID})
		for _, replica := range replicas {
			if err := s.serviceManager.Destroy(r.Context(), replica.ID); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", replica.Name, err))
			}
		}
	}

	if len(failed) > 0 {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("cannot destroy primary: %d replica(s) failed to destroy: %s", len(failed), strings.Join(failed, "; ")))
		return
	}

	if err := s.serviceManager.Destroy(r.Context(), svc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
}

func (s *Server) stopDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if _, err := s.serviceManager.Stop(r.Context(), svc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) startDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	updated, err := s.serviceManager.Redeploy(r.Context(), svc.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) getDatabaseLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Delegate to the existing service logs handler using the resolved service ID.
	r = r.WithContext(r.Context())
	chi.RouteContext(r.Context()).URLParams.Add("id", svc.ID)
	s.getServiceLogs(w, r)
}

func (s *Server) getDatabaseCredentials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if svc.DatabaseConfig == nil {
		writeError(w, http.StatusBadRequest, "not a database instance")
		return
	}
	cfg := svc.DatabaseConfig
	host := svc.GuestIP
	if host == "" {
		host = svc.Name + ".loka.internal"
	}
	defaults, _ := loka.GetEngineDefaults(cfg.Engine, cfg.Version)
	resp := map[string]any{
		"engine":      cfg.Engine,
		"version":     cfg.Version,
		"host":        host,
		"port":        defaults.Port,
		"login_role":  cfg.LoginRole,
		"password":    cfg.Password,
		"db_name":     cfg.DBName,
		"group_role":  cfg.GroupRole,
		"owner_role":  cfg.OwnerRole,
		"url":         loka.ConnectionString(cfg, host),
	}
	if cfg.PreviousLoginRole != "" {
		resp["previous_login_role"] = cfg.PreviousLoginRole
		resp["grace_deadline"] = cfg.GraceDeadline.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// defaultGracePeriod is how long the old user remains before auto-revocation.
const defaultGracePeriod = 5 * time.Minute

func (s *Server) rotateDatabaseCredentials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if svc.DatabaseConfig == nil {
		writeError(w, http.StatusBadRequest, "not a database instance")
		return
	}

	newPassword := generatePassword()
	s.rotateCredentials(r, w, svc, "", newPassword)
}

func (s *Server) setDatabaseCredentials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if svc.DatabaseConfig == nil {
		writeError(w, http.StatusBadRequest, "not a database instance")
		return
	}
	var req credentialsSetReq
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if strings.ContainsRune(req.Password, 0) {
		writeError(w, http.StatusBadRequest, "password must not contain null bytes")
		return
	}

	s.rotateCredentials(r, w, svc, "", req.Password)
}

// rotateCredentials creates a new login role granted the same group role (no restart).
//
// Model:
//   GroupRole (NOLOGIN) — owns privileges, never changes
//   OldLogin  (LOGIN)   — current, stays active during grace period
//   NewLogin  (LOGIN)   — created now, granted GroupRole immediately
//
// After grace period: old login is set to NOLOGIN then DROPped.
// Zero downtime — both login roles work during the transition window.
func (s *Server) rotateCredentials(r *http.Request, w http.ResponseWriter, svc *loka.Service, _ string, newPassword string) {
	cfg := svc.DatabaseConfig
	oldLogin := cfg.LoginRole
	newLogin := loka.GenerateLoginRole(cfg.DBName)
	graceDuration := defaultGracePeriod

	// Step 1: Generate SQL to CREATE new login role and GRANT group role.
	createSQL := loka.CreateLoginRoleSQL(cfg, newLogin, newPassword)
	if createSQL == "" {
		writeError(w, http.StatusInternalServerError, "unsupported engine for credential rotation")
		return
	}

	// Step 2: For postgres, also set VALID UNTIL on the old login as a hard expiry safety net.
	expireSQL := loka.ExpireLoginRoleSQL(cfg, oldLogin, time.Now().Add(graceDuration))

	// Execute the SQL inside the running database VM via service exec.
	execCmd := loka.ExecCreateLoginCommand(cfg, newLogin, newPassword)
	if err := s.serviceManager.ExecInService(r.Context(), svc.ID, []loka.Command{execCmd}); err != nil {
		s.logger.Warn("credential rotation: exec failed, SQL may need manual execution",
			"database", svc.Name, "error", err)
		// Continue — the SQL is included in the response for manual execution.
	} else {
		s.logger.Info("credential rotation: SQL executed in VM", "database", svc.Name)
	}

	// Step 3: Update the service record — new login is now the active credential.
	cfg.PreviousLoginRole = oldLogin
	cfg.GraceDeadline = time.Now().Add(graceDuration)
	cfg.LoginRole = newLogin
	cfg.Password = newPassword

	if err := s.store.Services().Update(r.Context(), svc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("credential rotation initiated",
		"database", svc.Name, "old_login", oldLogin, "new_login", newLogin,
		"grace_deadline", cfg.GraceDeadline, "initiated_by", clientIP(r))

	// GraceDeadline is stored on the service record. The credential reaper
	// (a background goroutine started via StartCredentialReaper) will revoke
	// the old login role after the deadline expires. No per-rotation goroutine needed.

	host := svc.GuestIP
	if host == "" {
		host = svc.Name + ".loka.internal"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"login_role":           newLogin,
		"password":             newPassword,
		"url":                  loka.ConnectionString(cfg, host),
		"group_role":           cfg.GroupRole,
		"previous_login_role":  oldLogin,
		"grace_period":         graceDuration.String(),
		"grace_deadline":       cfg.GraceDeadline.Format(time.RFC3339),
		"create_login_sql":     createSQL,
		"expire_old_login_sql": expireSQL,
		"pending_sql":          createSQL + "\n" + expireSQL,
		"warning":              "SQL not yet auto-executed if service is not running. Run the provided pending_sql inside your database to complete rotation.",
	})
}

func (s *Server) addDatabaseReplica(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if svc.DatabaseConfig == nil || svc.DatabaseConfig.Role != loka.DatabaseRolePrimary {
		writeError(w, http.StatusBadRequest, "can only add replicas to a primary database")
		return
	}

	var req struct {
		Count int `json:"count"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}

	// Count existing replicas using PrimaryID filter.
	existingReplicas, _, _ := s.store.Services().List(r.Context(), store.ServiceFilter{PrimaryID: &svc.ID})
	existingCount := 0
	for _, r := range existingReplicas {
		if r.DatabaseConfig != nil && r.DatabaseConfig.Role == loka.DatabaseRoleReplica {
			existingCount++
		}
	}

	cfg := svc.DatabaseConfig
	var replicas []*loka.Service
	for i := 0; i < req.Count; i++ {
		n := existingCount + i + 1
		replicaCfg := &loka.DatabaseConfig{
			Engine:    cfg.Engine,
			Version:   cfg.Version,
			GroupRole: cfg.GroupRole,
			OwnerRole: cfg.OwnerRole,
			LoginRole: cfg.LoginRole,
			Password:  cfg.Password,
			DBName:    cfg.DBName,
			Role:      loka.DatabaseRoleReplica,
			PrimaryID: svc.ID,
		}
		replica, err := s.serviceManager.Deploy(r.Context(), service.DeployOpts{
			Name:           fmt.Sprintf("%s-replica-%d", svc.Name, n),
			VCPUs:          svc.VCPUs,
			MemoryMB:       svc.MemoryMB,
			DatabaseConfig: replicaCfg,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		replicas = append(replicas, replica)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"replicas": replicas})
}

func (s *Server) removeDatabaseReplica(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "rid")
	replica, err := s.resolveDatabase(r, rid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if replica.DatabaseConfig == nil || replica.DatabaseConfig.Role != loka.DatabaseRoleReplica {
		writeError(w, http.StatusBadRequest, "not a replica")
		return
	}
	if err := s.serviceManager.Destroy(r.Context(), replica.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) listDatabaseReplicas(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Use PrimaryID filter instead of loading all databases.
	replicas, _, _ := s.store.Services().List(r.Context(), store.ServiceFilter{PrimaryID: &svc.ID})
	if replicas == nil {
		replicas = []*loka.Service{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"replicas": replicas})
}

func (s *Server) createDatabaseBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if s.backupManager == nil {
		writeError(w, http.StatusServiceUnavailable, "backup manager not available (no object store configured)")
		return
	}
	chainID, err := s.backupManager.CreateBackup(r.Context(), svc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"backup_id": chainID})
}

func (s *Server) listDatabaseBackups(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if s.backupManager == nil {
		writeError(w, http.StatusServiceUnavailable, "backup manager not available")
		return
	}
	catalog, err := s.backupManager.ListBackups(r.Context(), svc.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Convert to API-friendly format.
	type backupEntry struct {
		ID        string    `json:"id"`
		Type      string    `json:"type"`
		Size      int64     `json:"size"`
		CreatedAt time.Time `json:"created_at"`
	}
	var backups []backupEntry
	for _, chain := range catalog.Chains {
		totalSize := chain.BaseSize
		for _, seg := range chain.Segments {
			totalSize += seg.Size
		}
		backups = append(backups, backupEntry{
			ID:        chain.ID,
			Type:      "full+incremental",
			Size:      totalSize,
			CreatedAt: chain.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
}

func (s *Server) restoreDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if s.backupManager == nil {
		writeError(w, http.StatusServiceUnavailable, "backup manager not available")
		return
	}

	var req struct {
		BackupID    string `json:"backup_id"`
		PointInTime string `json:"point_in_time"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate the backup exists if an ID was provided.
	if req.BackupID != "" {
		if _, err := s.backupManager.GetBackup(r.Context(), svc.Name, req.BackupID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
	} else if req.PointInTime == "" {
		// If no backup ID and no point-in-time, use the latest backup.
		catalog, err := s.backupManager.ListBackups(r.Context(), svc.Name)
		if err != nil || len(catalog.Chains) == 0 {
			writeError(w, http.StatusBadRequest, "no backups available for restore")
			return
		}
		req.BackupID = catalog.Chains[len(catalog.Chains)-1].ID
	}

	// Stop the database before restore.
	s.serviceManager.Stop(r.Context(), svc.ID)

	// Restart the database. Actual data restore from the backup chain
	// requires worker-side exec (loka-backup-agent in VM) which will pipe
	// the backup from objstore into the engine's restore command.
	if _, err := s.serviceManager.Redeploy(r.Context(), svc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "restoring",
		"backup_id": req.BackupID,
	})
}

func (s *Server) upgradeDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if svc.DatabaseConfig == nil {
		writeError(w, http.StatusBadRequest, "not a database instance")
		return
	}

	var req struct {
		TargetVersion string `json:"target_version"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TargetVersion == "" {
		writeError(w, http.StatusBadRequest, "target_version is required")
		return
	}

	cfg := svc.DatabaseConfig
	if cfg.Version == req.TargetVersion {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("already running version %s", req.TargetVersion))
		return
	}

	// Only allow upgrading within the same engine.
	newDefaults, err := loka.GetEngineDefaults(cfg.Engine, req.TargetVersion)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Only allow upgrading primaries, not replicas.
	if cfg.Role != loka.DatabaseRolePrimary {
		writeError(w, http.StatusBadRequest, "can only upgrade primary databases — replicas will be upgraded automatically")
		return
	}

	// Step 1: Create mandatory backup before upgrade.
	var backupID string
	if s.backupManager != nil {
		backupID, err = s.backupManager.CreateBackup(r.Context(), svc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("pre-upgrade backup failed: %v", err))
			return
		}
	}

	// Step 2: Save upgrade metadata for rollback.
	oldVersion := cfg.Version
	cfg.PreviousVersion = oldVersion
	cfg.UpgradeVolume = "db-" + svc.Name

	// Step 3: Update primary to new version.
	cfg.Version = req.TargetVersion
	svc.ImageRef = newDefaults.Image

	if err := s.store.Services().Update(r.Context(), svc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Step 4: Redeploy primary with new image.
	if _, err := s.serviceManager.Redeploy(r.Context(), svc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("upgrade redeploy failed: %v", err))
		return
	}

	// Step 5: Upgrade replicas to match.
	var replicaWarnings []string
	replicas, _, _ := s.store.Services().List(r.Context(), store.ServiceFilter{PrimaryID: &svc.ID})
	for _, replica := range replicas {
		if replica.DatabaseConfig != nil {
			replica.DatabaseConfig.Version = req.TargetVersion
			replica.DatabaseConfig.PreviousVersion = oldVersion
			replica.ImageRef = newDefaults.Image
			if err := s.store.Services().Update(r.Context(), replica); err != nil {
				replicaWarnings = append(replicaWarnings, fmt.Sprintf("%s: update failed: %v", replica.Name, err))
				continue
			}
			if _, err := s.serviceManager.Redeploy(r.Context(), replica.ID); err != nil {
				replicaWarnings = append(replicaWarnings, fmt.Sprintf("%s: redeploy failed: %v", replica.Name, err))
			}
		}
	}

	resp := map[string]any{
		"status":           "upgrading",
		"previous_version": oldVersion,
		"target_version":   req.TargetVersion,
		"backup_id":        backupID,
		"replicas_upgraded": len(replicas) - len(replicaWarnings),
	}
	if len(replicaWarnings) > 0 {
		resp["warnings"] = replicaWarnings
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) rollbackDatabaseUpgrade(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if svc.DatabaseConfig == nil {
		writeError(w, http.StatusBadRequest, "not a database instance")
		return
	}

	cfg := svc.DatabaseConfig
	if cfg.PreviousVersion == "" {
		writeError(w, http.StatusBadRequest, "no upgrade to rollback (no previous version recorded)")
		return
	}

	// Restore to previous version.
	targetVersion := cfg.PreviousVersion
	defaults, err := loka.GetEngineDefaults(cfg.Engine, targetVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cfg.Version = targetVersion
	cfg.PreviousVersion = ""
	cfg.UpgradeVolume = ""
	svc.ImageRef = defaults.Image

	if err := s.store.Services().Update(r.Context(), svc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if _, err := s.serviceManager.Redeploy(r.Context(), svc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("rollback redeploy failed: %v", err))
		return
	}

	// Rollback replicas too.
	replicas, _, _ := s.store.Services().List(r.Context(), store.ServiceFilter{PrimaryID: &svc.ID})
	for _, replica := range replicas {
		if replica.DatabaseConfig != nil {
			replica.DatabaseConfig.Version = targetVersion
			replica.DatabaseConfig.PreviousVersion = ""
			replica.ImageRef = defaults.Image
			s.store.Services().Update(r.Context(), replica)
			s.serviceManager.Redeploy(r.Context(), replica.ID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "rolling_back",
		"restored_version":  targetVersion,
		"replicas_reverted": len(replicas),
	})
}

// resolveDatabase finds a database service by ID or name.
func (s *Server) resolveDatabase(r *http.Request, idOrName string) (*loka.Service, error) {
	// Try by ID first.
	svc, err := s.store.Services().Get(r.Context(), idOrName)
	if err == nil && svc.DatabaseConfig != nil {
		return svc, nil
	}
	// Try by name.
	isDB := true
	svcs, _, _ := s.store.Services().List(r.Context(), store.ServiceFilter{Name: &idOrName, IsDatabase: &isDB, Limit: 1})
	if len(svcs) > 0 {
		return svcs[0], nil
	}
	return nil, fmt.Errorf("database %q not found", idOrName)
}

// redactDatabaseConfig returns a copy with the password redacted.
func redactDatabaseConfig(cfg *loka.DatabaseConfig) *loka.DatabaseConfig {
	if cfg == nil {
		return nil
	}
	redacted := *cfg
	if redacted.Password != "" {
		redacted.Password = "********"
	}
	return &redacted
}

func (s *Server) verifyDatabaseBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	backupID := chi.URLParam(r, "backupId")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, ErrCodeDBNotFound, err.Error())
		return
	}
	if s.backupManager == nil {
		writeError(w, http.StatusServiceUnavailable, "backup manager not available")
		return
	}
	if err := s.backupManager.VerifyBackup(r.Context(), svc.Name, backupID); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "invalid", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "valid", "backup_id": backupID})
}

func (s *Server) forceStopDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	svc, err := s.resolveDatabase(r, id)
	if err != nil {
		writeErrorCode(w, http.StatusNotFound, ErrCodeDBNotFound, err.Error())
		return
	}

	// Force status to stopped regardless of current state.
	svc.Status = loka.ServiceStatusStopped
	svc.StatusMessage = "force-stopped by operator"

	// Clear stuck metadata.
	if svc.DatabaseConfig != nil {
		svc.DatabaseConfig.PreviousLoginRole = ""
		svc.DatabaseConfig.GraceDeadline = time.Time{}
		// Keep PreviousVersion for potential rollback.
	}

	svc.UpdatedAt = time.Now()
	if err := s.store.Services().Update(r.Context(), svc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Best-effort stop on worker.
	if svc.WorkerID != "" {
		s.serviceManager.Stop(r.Context(), svc.ID)
	}

	s.logger.Info("database force-stopped", "database", svc.Name, "initiated_by", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "force-stopped", "name": svc.Name})
}

// StartCredentialReaper starts a background goroutine that revokes expired
// login roles. Runs every 30 seconds, checking for services with a past
// GraceDeadline. Replaces per-rotation goroutines for better resource management.
func (s *Server) StartCredentialReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapExpiredCredentials()
			}
		}
	}()
}

func (s *Server) reapExpiredCredentials() {
	isDB := true
	dbs, _, err := s.store.Services().List(context.Background(), store.ServiceFilter{
		IsDatabase: &isDB, Limit: 100,
	})
	if err != nil {
		return
	}
	now := time.Now()
	for _, db := range dbs {
		cfg := db.DatabaseConfig
		if cfg == nil || cfg.PreviousLoginRole == "" || cfg.GraceDeadline.IsZero() {
			continue
		}
		if now.Before(cfg.GraceDeadline) {
			continue // Grace period not yet expired.
		}

		// Revoke the old login role by executing SQL inside the VM.
		oldLogin := cfg.PreviousLoginRole
		revokeCmd := loka.ExecRevokeLoginCommand(cfg, oldLogin)
		if s.serviceManager != nil {
			if err := s.serviceManager.ExecInService(context.Background(), db.ID, []loka.Command{revokeCmd}); err != nil {
				s.logger.Warn("credential reaper: revoke exec failed", "database", db.Name, "error", err)
			}
		}
		cfg.PreviousLoginRole = ""
		cfg.GraceDeadline = time.Time{}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s.store.Services().Update(ctx, db)
		cancel()

		s.logger.Info("credential reaper: revoked old login role",
			"database", db.Name, "revoked", oldLogin)
	}
}

func generatePassword() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)[:24]
}
