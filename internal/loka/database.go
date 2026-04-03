package loka

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// EncryptionKey is a package-level encryption key for credential encryption at rest.
// Set from config on startup. If empty, passwords are stored in plaintext (backwards compatible).
var EncryptionKey string

// EncryptPassword encrypts a password using AES-256-GCM.
// Returns base64(nonce + ciphertext). If no encryption key is set, returns plaintext.
func EncryptPassword(plaintext string) string {
	if EncryptionKey == "" || plaintext == "" {
		return plaintext
	}
	key := deriveKey(EncryptionKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return plaintext
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return plaintext
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + base64.StdEncoding.EncodeToString(ciphertext)
}

// DecryptPassword decrypts a password encrypted by EncryptPassword.
// If the value doesn't start with "enc:", it's returned as-is (plaintext).
func DecryptPassword(encrypted string) string {
	if EncryptionKey == "" || !strings.HasPrefix(encrypted, "enc:") {
		return encrypted
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encrypted, "enc:"))
	if err != nil {
		return encrypted
	}
	key := deriveKey(EncryptionKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return encrypted
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encrypted
	}
	if len(data) < gcm.NonceSize() {
		return encrypted
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Decryption failed — key may have changed since this password was encrypted.
		// Return a redacted placeholder instead of leaking ciphertext.
		slog.Warn("failed to decrypt database password — encryption key may have changed",
			"error", err)
		return "[encrypted — key mismatch]"
	}
	return string(plaintext)
}

func deriveKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// Validation patterns.
var (
	identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	dbNameRe     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	versionRe    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(-[a-zA-Z0-9]+)?$`)
)

// ValidateDBName checks that a database name is safe for use in SQL, file paths, and objstore keys.
func ValidateDBName(name string) error {
	if name == "" {
		return fmt.Errorf("database name cannot be empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("database name too long (max 63 chars)")
	}
	if !dbNameRe.MatchString(name) {
		return fmt.Errorf("database name %q contains invalid characters (allowed: a-z, 0-9, _, -)", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("database name %q contains path traversal", name)
	}
	return nil
}

// ValidateVersion checks that a version string is safe for use in Docker image tags.
func ValidateVersion(version string) error {
	if version == "" {
		return nil // empty = use default
	}
	if !versionRe.MatchString(version) {
		return fmt.Errorf("version %q has invalid format (expected: N.N or N.N-suffix)", version)
	}
	return nil
}

// sanitizeIdentifier strips characters that aren't safe for SQL identifiers.
// Only allows [a-zA-Z0-9_].
func SanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizePassword escapes single quotes for safe embedding in SQL string literals.
// 'pass' → 'pass', "it's" → "it''s"
func SanitizePassword(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// DatabaseConfig holds database-specific configuration for a service that
// represents a managed database instance. When set on a Service, the service
// is treated as a database (hidden from service list, shown in db list).
type DatabaseConfig struct {
	Engine    string       `json:"engine"`               // "postgres", "mysql", "redis"
	Version   string       `json:"version"`              // "16", "8.0", "7"
	DBName    string       `json:"db_name"`
	Role      DatabaseRole `json:"role"`                 // "primary", "replica", "sentinel"
	PrimaryID string       `json:"primary_id,omitempty"` // Service ID of primary (for replicas).
	Backup    *BackupConfig `json:"backup,omitempty"`

	// Role-based credential model:
	//
	//   GroupRole  — non-login role that owns all privileges (e.g., "loka_rw").
	//                Tables, schemas, etc. are owned by this role.
	//   OwnerRole — non-login role that owns database objects (e.g., "loka_owner").
	//   LoginRole — current login role with password (e.g., "loka_login_2026_03").
	//                Granted GroupRole so it inherits all privileges.
	//
	// On rotation, a NEW login role is created and granted GroupRole.
	// Old login role kept during grace period, then DROPped. No restart needed.
	// Privileges stay on GroupRole — login roles never own objects.
	GroupRole string `json:"group_role"`          // Non-login privilege role (e.g., "loka_rw").
	OwnerRole string `json:"owner_role"`          // Non-login owner of DB objects (e.g., "loka_owner").
	LoginRole string `json:"login_role"`          // Current login role name.
	Password  string `json:"password"`            // Current login role password.

	// Credential rotation state.
	PreviousLoginRole string    `json:"previous_login_role,omitempty"` // Old login role pending revocation.
	GraceDeadline     time.Time `json:"grace_deadline,omitempty"`     // When old login role gets DROPped.

	// Upgrade metadata — set during version migration for rollback support.
	PreviousVersion string `json:"previous_version,omitempty"` // Version before upgrade.
	UpgradeVolume   string `json:"upgrade_volume,omitempty"`   // Pre-upgrade volume name for rollback.
}

// DatabaseRole describes the role of a database instance in a replication topology.
type DatabaseRole string

const (
	DatabaseRolePrimary  DatabaseRole = "primary"
	DatabaseRoleReplica  DatabaseRole = "replica"
	DatabaseRoleSentinel DatabaseRole = "sentinel" // Redis Sentinel only.
)

// BackupConfig controls automated database backups.
type BackupConfig struct {
	Enabled         bool   `json:"enabled"`
	Schedule        string `json:"schedule"`          // Cron expression (default: "0 */6 * * *").
	Retention       int    `json:"retention"`          // Days to keep backups (default: 7).
	WAL             bool   `json:"wal"`                // WAL/binlog archiving for PITR (postgres/mysql).
	IntervalSeconds int    `json:"interval_seconds"`   // Precomputed from Schedule at creation time.
}

// ParseScheduleInterval converts common cron schedule patterns to a duration in seconds.
func ParseScheduleInterval(schedule string) int {
	switch {
	case strings.Contains(schedule, "0 0 * * *"):
		return 86400 // daily
	case strings.Contains(schedule, "*/12"):
		return 43200 // 12 hours
	case strings.Contains(schedule, "*/6"):
		return 21600 // 6 hours
	case strings.Contains(schedule, "*/1 "):
		return 3600 // 1 hour
	default:
		return 21600 // default 6 hours
	}
}

// EngineDefaults holds per-engine configuration defaults.
type EngineDefaults struct {
	Image      string
	Port       int
	DataDir    string // Mount path inside VM for persistent data.
	BackupCmd  string // Engine-native backup command.
	RestoreCmd string // Engine-native restore command.
}

// SupportedEngines lists valid database engine names.
var SupportedEngines = []string{"postgres", "mysql", "redis"}

// DefaultVersions maps engines to their default version.
var DefaultVersions = map[string]string{
	"postgres": "16",
	"mysql":    "8.0",
	"redis":    "7",
}

// GetEngineDefaults returns the default configuration for a database engine.
func GetEngineDefaults(engine, version string) (EngineDefaults, error) {
	if version == "" {
		version = DefaultVersions[engine]
	}
	switch engine {
	case "postgres":
		return EngineDefaults{
			Image:      fmt.Sprintf("postgres:%s", version),
			Port:       5432,
			DataDir:    "/var/lib/postgresql/data",
			BackupCmd:  "pg_basebackup",
			RestoreCmd: "pg_restore",
		}, nil
	case "mysql":
		return EngineDefaults{
			Image:      fmt.Sprintf("mysql:%s", version),
			Port:       3306,
			DataDir:    "/var/lib/mysql",
			BackupCmd:  "mysqldump --single-transaction",
			RestoreCmd: "mysql",
		}, nil
	case "redis":
		return EngineDefaults{
			Image:      fmt.Sprintf("redis:%s", version),
			Port:       6379,
			DataDir:    "/data",
			BackupCmd:  "redis-cli BGSAVE",
			RestoreCmd: "redis-cli",
		}, nil
	default:
		return EngineDefaults{}, fmt.Errorf("unsupported database engine: %s (supported: postgres, mysql, redis)", engine)
	}
}

// DatabaseEnv returns the environment variables needed to initialize a database.
// Uses the LoginRole as the initial superuser that postgres/mysql creates on first boot.
func DatabaseEnv(cfg *DatabaseConfig) map[string]string {
	switch cfg.Engine {
	case "postgres":
		env := map[string]string{
			"POSTGRES_USER":     cfg.LoginRole,
			"POSTGRES_PASSWORD": cfg.Password,
			"POSTGRES_DB":       cfg.DBName,
		}
		if cfg.Role == DatabaseRolePrimary {
			env["POSTGRES_INITDB_ARGS"] = "--data-checksums"
		}
		return env
	case "mysql":
		env := map[string]string{
			"MYSQL_ROOT_PASSWORD": cfg.Password,
			"MYSQL_USER":         cfg.LoginRole,
			"MYSQL_PASSWORD":     cfg.Password,
			"MYSQL_DATABASE":     cfg.DBName,
		}
		return env
	case "redis":
		// Don't use REDIS_PASSWORD (sets requirepass, incompatible with ACL rotation).
		// ACL is configured via InitRolesSQL after boot.
		return nil
	default:
		return nil
	}
}

// DatabaseArgs returns command arguments for the database engine.
func DatabaseArgs(cfg *DatabaseConfig) []string {
	switch cfg.Engine {
	case "redis":
		// No --requirepass: we use ACL mode for multi-user credential rotation.
		// Initial password is set via ACL SETUSER in InitRolesSQL.
		return nil
	default:
		return nil
	}
}

// InitRolesSQL returns the SQL to set up the group-role privilege model on first boot.
// This runs after the DB engine creates the initial superuser (LoginRole).
//
// Layout:
//   loka_owner  — NOLOGIN, owns database objects (tables, schemas)
//   loka_rw     — NOLOGIN, has read/write privileges, granted to login roles
//   <LoginRole> — LOGIN, the initial user, member of loka_rw
func InitRolesSQL(cfg *DatabaseConfig) string {
	owner := SanitizeIdentifier(cfg.OwnerRole)
	group := SanitizeIdentifier(cfg.GroupRole)
	login := SanitizeIdentifier(cfg.LoginRole)
	pw := SanitizePassword(cfg.Password)

	switch cfg.Engine {
	case "postgres":
		return fmt.Sprintf(`
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
    CREATE ROLE %s NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
    CREATE ROLE %s NOLOGIN;
  END IF;
  GRANT %s TO %s;
  ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s;
  ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO %s;
END $$;`,
			owner, owner,
			group, group,
			group, login,
			owner, group,
			owner, group,
		)
	case "mysql":
		db := SanitizeIdentifier(cfg.DBName)
		return fmt.Sprintf(`
CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';
GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%%';
FLUSH PRIVILEGES;`,
			login, pw, db, login,
		)
	case "redis":
		return fmt.Sprintf(
			"ACL SETUSER default off\n"+
				"ACL SETUSER %s on >%s ~* +@all",
			login, SanitizePassword(cfg.Password))
	default:
		return ""
	}
}

// CreateLoginRoleSQL returns the SQL to create a new login role and grant it
// the group privilege role. No restart needed — executes on the running instance.
func CreateLoginRoleSQL(cfg *DatabaseConfig, newLogin, newPassword string) string {
	login := SanitizeIdentifier(newLogin)
	pw := SanitizePassword(newPassword)
	group := SanitizeIdentifier(cfg.GroupRole)
	db := SanitizeIdentifier(cfg.DBName)

	switch cfg.Engine {
	case "postgres":
		return fmt.Sprintf(
			`CREATE ROLE %s WITH LOGIN PASSWORD '%s'; GRANT %s TO %s;`,
			login, pw, group, login)
	case "mysql":
		return fmt.Sprintf(
			`CREATE USER '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%%'; FLUSH PRIVILEGES;`,
			login, pw, db, login)
	case "redis":
		return fmt.Sprintf(`ACL SETUSER %s on >%s ~* +@all`, login, SanitizePassword(newPassword))
	default:
		return ""
	}
}

// RevokeLoginRoleSQL returns SQL to disable and drop an old login role after grace.
// Uses a two-step approach: first disable login, then drop.
func RevokeLoginRoleSQL(cfg *DatabaseConfig, oldLogin string) string {
	login := SanitizeIdentifier(oldLogin)
	switch cfg.Engine {
	case "postgres":
		return fmt.Sprintf(
			`ALTER ROLE %s NOLOGIN; DROP ROLE IF EXISTS %s;`,
			login, login)
	case "mysql":
		return fmt.Sprintf(`DROP USER IF EXISTS '%s'@'%%';`, login)
	case "redis":
		return fmt.Sprintf(`ACL DELUSER %s`, login)
	default:
		return ""
	}
}

// ExpireLoginRoleSQL returns SQL to set a hard password expiry on an old login role.
// Postgres: VALID UNTIL enforced only for password-based auth.
func ExpireLoginRoleSQL(cfg *DatabaseConfig, oldLogin string, deadline time.Time) string {
	login := SanitizeIdentifier(oldLogin)
	switch cfg.Engine {
	case "postgres":
		return fmt.Sprintf(`ALTER ROLE %s VALID UNTIL '%s';`,
			login, deadline.UTC().Format("2006-01-02 15:04:05+00"))
	case "mysql":
		// MySQL 8.0 supports ALTER USER ... PASSWORD EXPIRE.
		// For a timed expiry, we rely on the background DROP.
		return ""
	case "redis":
		return ""
	default:
		return ""
	}
}

// GenerateLoginRole creates a unique login role name using crypto/rand.
// Uses 8 bytes (64 bits) of randomness for the suffix.
func GenerateLoginRole(dbName string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%s_login_%s", SanitizeIdentifier(dbName), hex.EncodeToString(b))
}

// ExecCreateLoginCommand returns the shell command to execute the CreateLoginRoleSQL
// inside the database VM using the engine's native client.
func ExecCreateLoginCommand(cfg *DatabaseConfig, newLogin, newPassword string) Command {
	sql := CreateLoginRoleSQL(cfg, newLogin, newPassword)
	switch cfg.Engine {
	case "postgres":
		return Command{
			Command: "sh",
			Args: []string{"-c", fmt.Sprintf("psql -U %s -d %s -c %q", SanitizeIdentifier(cfg.LoginRole), SanitizeIdentifier(cfg.DBName), sql)},
		}
	case "mysql":
		return Command{
			Command: "sh",
			Args: []string{"-c", fmt.Sprintf("mysql -u root -p\"$MYSQL_ROOT_PASSWORD\" -e %q", sql)},
		}
	case "redis":
		return Command{
			Command: "sh",
			Args: []string{"-c", fmt.Sprintf("redis-cli %s", sql)},
		}
	default:
		return Command{Command: "echo", Args: []string{"unsupported engine: " + cfg.Engine}}
	}
}

// ExecRevokeLoginCommand returns the shell command to execute the RevokeLoginRoleSQL.
func ExecRevokeLoginCommand(cfg *DatabaseConfig, oldLogin string) Command {
	sql := RevokeLoginRoleSQL(cfg, oldLogin)
	switch cfg.Engine {
	case "postgres":
		return Command{
			Command: "sh",
			Args: []string{"-c", fmt.Sprintf("psql -U %s -d %s -c %q", SanitizeIdentifier(cfg.LoginRole), SanitizeIdentifier(cfg.DBName), sql)},
		}
	case "mysql":
		return Command{
			Command: "sh",
			Args: []string{"-c", fmt.Sprintf("mysql -u root -p\"$MYSQL_ROOT_PASSWORD\" -e %q", sql)},
		}
	case "redis":
		return Command{
			Command: "sh",
			Args: []string{"-c", fmt.Sprintf("redis-cli %s", sql)},
		}
	default:
		return Command{Command: "echo", Args: []string{"unsupported engine"}}
	}
}

// ConnectionString returns a connection URL for the database.
func ConnectionString(cfg *DatabaseConfig, host string) string {
	switch cfg.Engine {
	case "postgres":
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
			cfg.LoginRole, cfg.Password, host, 5432, cfg.DBName)
	case "mysql":
		return fmt.Sprintf("mysql://%s:%s@%s:%d/%s",
			cfg.LoginRole, cfg.Password, host, 3306, cfg.DBName)
	case "redis":
		if cfg.Password != "" {
			// Redis 6+ ACL: AUTH <username> <password>
			return fmt.Sprintf("redis://%s:%s@%s:%d", cfg.LoginRole, cfg.Password, host, 6379)
		}
		return fmt.Sprintf("redis://%s:%d", host, 6379)
	default:
		return ""
	}
}
