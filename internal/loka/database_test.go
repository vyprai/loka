package loka

import (
	"testing"
	"time"
)

func TestGetEngineDefaults(t *testing.T) {
	tests := []struct {
		engine  string
		version string
		image   string
		port    int
		dataDir string
		wantErr bool
	}{
		{"postgres", "", "postgres:16", 5432, "/var/lib/postgresql/data", false},
		{"postgres", "15", "postgres:15", 5432, "/var/lib/postgresql/data", false},
		{"mysql", "", "mysql:8.0", 3306, "/var/lib/mysql", false},
		{"mysql", "5.7", "mysql:5.7", 3306, "/var/lib/mysql", false},
		{"redis", "", "redis:7", 6379, "/data", false},
		{"redis", "6", "redis:6", 6379, "/data", false},
		{"unknown", "", "", 0, "", true},
		{"", "", "", 0, "", true},
	}

	for _, tt := range tests {
		name := tt.engine
		if tt.version != "" {
			name += ":" + tt.version
		}
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			got, err := GetEngineDefaults(tt.engine, tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for engine %q, got nil", tt.engine)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Image != tt.image {
				t.Errorf("Image = %q, want %q", got.Image, tt.image)
			}
			if got.Port != tt.port {
				t.Errorf("Port = %d, want %d", got.Port, tt.port)
			}
			if got.DataDir != tt.dataDir {
				t.Errorf("DataDir = %q, want %q", got.DataDir, tt.dataDir)
			}
			if got.BackupCmd == "" {
				t.Error("BackupCmd should not be empty")
			}
			if got.RestoreCmd == "" {
				t.Error("RestoreCmd should not be empty")
			}
		})
	}
}

func TestGetEngineDefaults_VersionDefaulting(t *testing.T) {
	for engine, defaultVer := range DefaultVersions {
		t.Run(engine, func(t *testing.T) {
			got, err := GetEngineDefaults(engine, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			expected := engine + ":" + defaultVer
			if got.Image != expected {
				t.Errorf("Image = %q, want %q (default version)", got.Image, expected)
			}
		})
	}
}

func TestDatabaseEnv_Postgres(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:   "postgres",
		LoginRole: "myuser",
		Password: "mypass",
		DBName:   "mydb",
		Role:     DatabaseRolePrimary,
	}
	env := DatabaseEnv(cfg)
	if env["POSTGRES_USER"] != "myuser" {
		t.Errorf("POSTGRES_USER = %q, want %q", env["POSTGRES_USER"], "myuser")
	}
	if env["POSTGRES_PASSWORD"] != "mypass" {
		t.Errorf("POSTGRES_PASSWORD = %q, want %q", env["POSTGRES_PASSWORD"], "mypass")
	}
	if env["POSTGRES_DB"] != "mydb" {
		t.Errorf("POSTGRES_DB = %q, want %q", env["POSTGRES_DB"], "mydb")
	}
	// Primary should have INITDB_ARGS for checksums.
	if env["POSTGRES_INITDB_ARGS"] != "--data-checksums" {
		t.Errorf("POSTGRES_INITDB_ARGS = %q, want --data-checksums", env["POSTGRES_INITDB_ARGS"])
	}
}

func TestDatabaseEnv_PostgresReplica(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:   "postgres",
		LoginRole: "myuser",
		Password: "mypass",
		DBName:   "mydb",
		Role:     DatabaseRoleReplica,
	}
	env := DatabaseEnv(cfg)
	// Replica should NOT have INITDB_ARGS.
	if _, ok := env["POSTGRES_INITDB_ARGS"]; ok {
		t.Error("replica should not have POSTGRES_INITDB_ARGS")
	}
}

func TestDatabaseEnv_MySQL(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:   "mysql",
		LoginRole: "app",
		Password: "secret",
		DBName:   "appdb",
	}
	env := DatabaseEnv(cfg)
	if env["MYSQL_ROOT_PASSWORD"] != "secret" {
		t.Errorf("MYSQL_ROOT_PASSWORD = %q, want %q", env["MYSQL_ROOT_PASSWORD"], "secret")
	}
	if env["MYSQL_USER"] != "app" {
		t.Errorf("MYSQL_USER = %q, want %q", env["MYSQL_USER"], "app")
	}
	if env["MYSQL_PASSWORD"] != "secret" {
		t.Errorf("MYSQL_PASSWORD = %q, want %q", env["MYSQL_PASSWORD"], "secret")
	}
	if env["MYSQL_DATABASE"] != "appdb" {
		t.Errorf("MYSQL_DATABASE = %q, want %q", env["MYSQL_DATABASE"], "appdb")
	}
}

func TestDatabaseEnv_Redis(t *testing.T) {
	// Redis uses ACL mode, not REDIS_PASSWORD env. Env should be nil.
	cfg := &DatabaseConfig{Engine: "redis", Password: "redispass"}
	env := DatabaseEnv(cfg)
	if env != nil {
		t.Errorf("expected nil env for redis (ACL mode), got %v", env)
	}
}

func TestDatabaseEnv_RedisNoPassword(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "redis", Password: ""}
	env := DatabaseEnv(cfg)
	if env != nil {
		t.Errorf("expected nil env for redis without password, got %v", env)
	}
}

func TestDatabaseEnv_UnknownEngine(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "unknown"}
	env := DatabaseEnv(cfg)
	if env != nil {
		t.Errorf("expected nil env for unknown engine, got %v", env)
	}
}

func TestDatabaseArgs_RedisWithPassword(t *testing.T) {
	// Redis uses ACL mode, no --requirepass args.
	cfg := &DatabaseConfig{Engine: "redis", Password: "secret"}
	args := DatabaseArgs(cfg)
	if args != nil {
		t.Errorf("expected nil args for redis (ACL mode), got %v", args)
	}
}

func TestDatabaseArgs_RedisNoPassword(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "redis", Password: ""}
	args := DatabaseArgs(cfg)
	if args != nil {
		t.Errorf("expected nil args, got %v", args)
	}
}

func TestDatabaseArgs_Postgres(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres", Password: "pass"}
	args := DatabaseArgs(cfg)
	if args != nil {
		t.Errorf("expected nil args for postgres, got %v", args)
	}
}

func TestDatabaseArgs_MySQL(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "mysql", Password: "pass"}
	args := DatabaseArgs(cfg)
	if args != nil {
		t.Errorf("expected nil args for mysql, got %v", args)
	}
}

func TestConnectionString(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *DatabaseConfig
		host   string
		expect string
	}{
		{
			"postgres",
			&DatabaseConfig{Engine: "postgres", LoginRole: "user", Password: "pass", DBName: "db"},
			"10.0.0.1",
			"postgres://user:pass@10.0.0.1:5432/db",
		},
		{
			"mysql",
			&DatabaseConfig{Engine: "mysql", LoginRole: "user", Password: "pass", DBName: "db"},
			"10.0.0.2",
			"mysql://user:pass@10.0.0.2:3306/db",
		},
		{
			"redis_with_password",
			&DatabaseConfig{Engine: "redis", LoginRole: "myuser", Password: "secret"},
			"10.0.0.3",
			"redis://myuser:secret@10.0.0.3:6379",
		},
		{
			"redis_no_password",
			&DatabaseConfig{Engine: "redis"},
			"10.0.0.4",
			"redis://10.0.0.4:6379",
		},
		{
			"unknown",
			&DatabaseConfig{Engine: "unknown"},
			"10.0.0.5",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConnectionString(tt.cfg, tt.host)
			if got != tt.expect {
				t.Errorf("ConnectionString = %q, want %q", got, tt.expect)
			}
		})
	}
}

// --- Role-based credential model tests ---

func TestInitRolesSQL_Postgres(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:    "postgres",
		DBName:    "mydb",
		GroupRole: "mydb_rw",
		OwnerRole: "mydb_owner",
		LoginRole: "mydb_login",
		Password:  "secret",
	}
	sql := InitRolesSQL(cfg)
	if sql == "" {
		t.Fatal("expected non-empty SQL")
	}
	for _, want := range []string{"mydb_owner", "mydb_rw", "mydb_login", "NOLOGIN", "GRANT"} {
		if !contains(sql, want) {
			t.Errorf("InitRolesSQL missing %q", want)
		}
	}
}

func TestInitRolesSQL_MySQL(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:    "mysql",
		DBName:    "appdb",
		LoginRole: "app_login",
		Password:  "secret",
	}
	sql := InitRolesSQL(cfg)
	if sql == "" {
		t.Fatal("expected non-empty SQL")
	}
	for _, want := range []string{"app_login", "secret", "appdb", "GRANT"} {
		if !contains(sql, want) {
			t.Errorf("InitRolesSQL missing %q", want)
		}
	}
}

func TestInitRolesSQL_Redis(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:    "redis",
		LoginRole: "cache_login",
		Password:  "redispass",
	}
	sql := InitRolesSQL(cfg)
	if sql == "" {
		t.Fatal("expected non-empty ACL command")
	}
	// Should disable default user and create named login.
	for _, want := range []string{"default off", "cache_login", "redispass", "+@all"} {
		if !contains(sql, want) {
			t.Errorf("InitRolesSQL missing %q in: %s", want, sql)
		}
	}
}

func TestCreateLoginRoleSQL_Postgres(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres", GroupRole: "app_rw", DBName: "db"}
	sql := CreateLoginRoleSQL(cfg, "app_login_new", "newpass")
	for _, want := range []string{"CREATE ROLE", "app_login_new", "LOGIN", "newpass", "GRANT", "app_rw"} {
		if !contains(sql, want) {
			t.Errorf("CreateLoginRoleSQL missing %q", want)
		}
	}
}

func TestCreateLoginRoleSQL_MySQL(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "mysql", DBName: "appdb"}
	sql := CreateLoginRoleSQL(cfg, "new_login", "pw123")
	for _, want := range []string{"CREATE USER", "new_login", "pw123", "GRANT", "appdb"} {
		if !contains(sql, want) {
			t.Errorf("CreateLoginRoleSQL missing %q", want)
		}
	}
}

func TestCreateLoginRoleSQL_Redis(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "redis"}
	sql := CreateLoginRoleSQL(cfg, "new_user", "pw")
	for _, want := range []string{"ACL SETUSER", "new_user", "pw", "+@all"} {
		if !contains(sql, want) {
			t.Errorf("CreateLoginRoleSQL missing %q", want)
		}
	}
}

func TestCreateLoginRoleSQL_Unknown(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "unknown"}
	if sql := CreateLoginRoleSQL(cfg, "u", "p"); sql != "" {
		t.Errorf("expected empty SQL for unknown engine, got %q", sql)
	}
}

func TestRevokeLoginRoleSQL_Postgres(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres", DBName: "db"}
	sql := RevokeLoginRoleSQL(cfg, "old_login")
	for _, want := range []string{"NOLOGIN", "DROP ROLE", "old_login"} {
		if !contains(sql, want) {
			t.Errorf("RevokeLoginRoleSQL missing %q", want)
		}
	}
}

func TestRevokeLoginRoleSQL_MySQL(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "mysql"}
	sql := RevokeLoginRoleSQL(cfg, "old_login")
	for _, want := range []string{"DROP USER", "old_login"} {
		if !contains(sql, want) {
			t.Errorf("RevokeLoginRoleSQL missing %q", want)
		}
	}
}

func TestRevokeLoginRoleSQL_Redis(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "redis"}
	sql := RevokeLoginRoleSQL(cfg, "old_user")
	if !contains(sql, "ACL DELUSER") || !contains(sql, "old_user") {
		t.Errorf("RevokeLoginRoleSQL = %q, want ACL DELUSER old_user", sql)
	}
}

func TestExpireLoginRoleSQL_Postgres(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres"}
	deadline := time.Date(2026, 4, 15, 23, 59, 59, 0, time.UTC)
	sql := ExpireLoginRoleSQL(cfg, "old_login", deadline)
	for _, want := range []string{"VALID UNTIL", "old_login", "2026-04-15"} {
		if !contains(sql, want) {
			t.Errorf("ExpireLoginRoleSQL missing %q in: %s", want, sql)
		}
	}
}

func TestExpireLoginRoleSQL_MySQL(t *testing.T) {
	// MySQL doesn't support VALID UNTIL — returns empty.
	cfg := &DatabaseConfig{Engine: "mysql"}
	sql := ExpireLoginRoleSQL(cfg, "old", time.Now())
	if sql != "" {
		t.Errorf("expected empty for mysql, got %q", sql)
	}
}

func TestExpireLoginRoleSQL_Redis(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "redis"}
	sql := ExpireLoginRoleSQL(cfg, "old", time.Now())
	if sql != "" {
		t.Errorf("expected empty for redis, got %q", sql)
	}
}

func TestGenerateLoginRole(t *testing.T) {
	role := GenerateLoginRole("mydb")
	if role == "" {
		t.Error("expected non-empty login role name")
	}
	if !contains(role, "mydb_login_") {
		t.Errorf("GenerateLoginRole = %q, want prefix mydb_login_", role)
	}
}

func TestGenerateLoginRole_Unique(t *testing.T) {
	r1 := GenerateLoginRole("db")
	r2 := GenerateLoginRole("db")
	// crypto/rand suffix — should be unique without sleeping.
	if r1 == r2 {
		t.Errorf("expected unique role names, both got %q", r1)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- Edge case tests ---

func TestCreateLoginRoleSQL_SpecialCharsInPassword(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres", GroupRole: "app_rw", DBName: "db"}
	// Password with SQL injection attempt — single quotes should be escaped.
	sql := CreateLoginRoleSQL(cfg, "user1", "pass'; DROP TABLE users; --")
	if sql == "" {
		t.Fatal("expected non-empty SQL")
	}
	// Single quote escaped: ' → ''
	if !containsStr(sql, "pass''; DROP TABLE users; --") {
		t.Errorf("password not properly escaped in SQL: %s", sql)
	}
	// Should NOT contain the raw injection.
	if containsStr(sql, "PASSWORD 'pass';") {
		t.Error("password single quote NOT escaped — SQL injection possible")
	}
}

func TestInitRolesSQL_SpecialCharsInRoleName(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:    "postgres",
		GroupRole: "app_rw",
		OwnerRole: "app_owner",
		LoginRole: "user_with'quote",
		Password:  "pass",
		DBName:    "db",
	}
	sql := InitRolesSQL(cfg)
	if sql == "" {
		t.Fatal("expected non-empty SQL")
	}
	// Documents that special chars flow through — trusted VM execution only.
}

func TestRevokeLoginRoleSQL_SpecialChars(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres", DBName: "db"}
	sql := RevokeLoginRoleSQL(cfg, "old'; DROP ROLE admin; --")
	if sql == "" {
		t.Fatal("expected non-empty SQL")
	}
}

func TestDatabaseEnv_PostgresEmptyPassword(t *testing.T) {
	cfg := &DatabaseConfig{
		Engine:    "postgres",
		LoginRole: "user",
		Password:  "",
		DBName:    "db",
		Role:      DatabaseRolePrimary,
	}
	env := DatabaseEnv(cfg)
	// Empty password should still produce the env var (postgres requires it for init).
	if env["POSTGRES_PASSWORD"] != "" {
		t.Errorf("POSTGRES_PASSWORD = %q, want empty string", env["POSTGRES_PASSWORD"])
	}
	if env["POSTGRES_USER"] != "user" {
		t.Errorf("POSTGRES_USER = %q, want user", env["POSTGRES_USER"])
	}
}

func TestDatabaseEnv_MySQLEmptyPassword(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "mysql", LoginRole: "root", Password: "", DBName: "db"}
	env := DatabaseEnv(cfg)
	if env["MYSQL_ROOT_PASSWORD"] != "" {
		t.Errorf("MYSQL_ROOT_PASSWORD = %q, want empty", env["MYSQL_ROOT_PASSWORD"])
	}
}

func TestConnectionString_EmptyCredentials(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "postgres", LoginRole: "", Password: "", DBName: "db"}
	url := ConnectionString(cfg, "host")
	if url != "postgres://:@host:5432/db" {
		t.Errorf("URL = %q, want postgres://:@host:5432/db", url)
	}
}

func TestConnectionString_RedisEmptyLoginRole(t *testing.T) {
	cfg := &DatabaseConfig{Engine: "redis", LoginRole: "", Password: "pass"}
	url := ConnectionString(cfg, "host")
	if url != "redis://:pass@host:6379" {
		t.Errorf("URL = %q, want redis://:pass@host:6379", url)
	}
}

// --- Sanitize/Validate edge cases ---

func TestSanitizeIdentifier_Empty(t *testing.T) {
	if got := SanitizeIdentifier(""); got != "" {
		t.Errorf("SanitizeIdentifier('') = %q, want empty", got)
	}
}

func TestSanitizeIdentifier_OnlySpecialChars(t *testing.T) {
	if got := SanitizeIdentifier("!@#$%^&*()"); got != "" {
		t.Errorf("SanitizeIdentifier('!@#$%%') = %q, want empty", got)
	}
}

func TestSanitizeIdentifier_MixedChars(t *testing.T) {
	if got := SanitizeIdentifier("my-role_123!"); got != "myrole_123" {
		t.Errorf("SanitizeIdentifier = %q, want 'myrole_123'", got)
	}
}

func TestSanitizePassword_NoQuotes(t *testing.T) {
	if got := SanitizePassword("password123"); got != "password123" {
		t.Errorf("SanitizePassword no quotes = %q, want unchanged", got)
	}
}

func TestSanitizePassword_MultipleQuotes(t *testing.T) {
	if got := SanitizePassword("a'b'c"); got != "a''b''c" {
		t.Errorf("SanitizePassword = %q, want a''b''c", got)
	}
}

func TestValidateDBName_ExactlyMaxLength(t *testing.T) {
	name := "a" + string(make([]byte, 62)) // would be 63 bytes but may contain nulls
	// Use a proper 63-char string.
	name = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0" // 63 chars
	if err := ValidateDBName(name); err != nil {
		t.Errorf("63-char name should pass: %v", err)
	}
}

func TestValidateDBName_OverMaxLength(t *testing.T) {
	name := "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz01" // 64 chars
	if err := ValidateDBName(name); err == nil {
		t.Error("64-char name should fail")
	}
}

func TestValidateVersion_Empty(t *testing.T) {
	if err := ValidateVersion(""); err != nil {
		t.Errorf("empty version should pass: %v", err)
	}
}

func TestValidateVersion_ValidFormats(t *testing.T) {
	for _, v := range []string{"16", "8.0", "5.7", "7", "16-alpine", "3.12-slim"} {
		if err := ValidateVersion(v); err != nil {
			t.Errorf("ValidateVersion(%q) = %v, want nil", v, err)
		}
	}
}

func TestValidateVersion_InvalidFormats(t *testing.T) {
	for _, v := range []string{"abc", "../etc", "16 && malicious"} {
		if err := ValidateVersion(v); err == nil {
			t.Errorf("ValidateVersion(%q) should fail", v)
		}
	}
}

func TestSupportedEngines(t *testing.T) {
	if len(SupportedEngines) != 3 {
		t.Errorf("expected 3 supported engines, got %d", len(SupportedEngines))
	}
	expected := map[string]bool{"postgres": true, "mysql": true, "redis": true}
	for _, e := range SupportedEngines {
		if !expected[e] {
			t.Errorf("unexpected engine %q in SupportedEngines", e)
		}
	}
}

func TestDecryptPassword_WrongKey(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	EncryptionKey = "key-one"
	encrypted := EncryptPassword("my-secret-password")

	EncryptionKey = "key-two"
	decrypted := DecryptPassword(encrypted)

	// Wrong key can't decrypt — returns encrypted blob as-is.
	if decrypted == "my-secret-password" {
		t.Error("decrypted with wrong key should NOT return plaintext")
	}
	if decrypted != encrypted {
		t.Error("expected encrypted blob returned as-is on wrong key")
	}
}

func TestDecryptPassword_MixedState(t *testing.T) {
	oldKey := EncryptionKey
	defer func() { EncryptionKey = oldKey }()

	// Plaintext (no encryption key set).
	EncryptionKey = ""
	plain := EncryptPassword("plain-password")
	if plain != "plain-password" {
		t.Error("no encryption key: password should be plaintext")
	}

	// Now enable encryption.
	EncryptionKey = "my-key"
	encrypted := EncryptPassword("encrypted-password")

	// Both should be readable.
	if DecryptPassword(plain) != "plain-password" {
		t.Error("plaintext password should be readable even with key set")
	}
	if DecryptPassword(encrypted) != "encrypted-password" {
		t.Error("encrypted password should be decryptable")
	}
}

func TestDefaultVersions(t *testing.T) {
	for _, engine := range SupportedEngines {
		if DefaultVersions[engine] == "" {
			t.Errorf("no default version for engine %q", engine)
		}
	}
}
