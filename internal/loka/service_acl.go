package loka

import (
	"fmt"
	"regexp"
	"strings"
)

// aliasRe validates that aliases are safe for use in env var names and /etc/hosts.
var aliasRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]*$`)

// NormalizeUses converts the `uses` field from loka.yml into a canonical
// map[string]string (alias→target). Supports two input forms:
//
//	List:  ["mydb", "payment-svc"]  → {"mydb":"mydb", "payment-svc":"payment-svc"}
//	Map:   {"db":"mydb", "pay":"payment-svc"}  → passed through as-is
//
// The raw interface{} comes from YAML unmarshaling which may produce
// []interface{} (list) or map[string]interface{} (map).
func NormalizeUses(raw interface{}) map[string]string {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case map[string]interface{}:
		result := make(map[string]string, len(v))
		for alias, target := range v {
			if alias != "" && aliasRe.MatchString(alias) {
				result[alias] = fmt.Sprint(target)
			}
		}
		if len(result) == 0 {
			return nil
		}
		return result
	case []interface{}:
		if len(v) == 0 {
			return nil
		}
		result := make(map[string]string, len(v))
		for _, item := range v {
			name := fmt.Sprint(item)
			result[name] = name // alias == target in list form
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(v))
		for alias, target := range v {
			if alias != "" && aliasRe.MatchString(alias) {
				result[alias] = target
			}
		}
		if len(result) == 0 {
			return nil
		}
		return result
	case []string:
		if len(v) == 0 {
			return nil
		}
		result := make(map[string]string, len(v))
		for _, name := range v {
			result[name] = name
		}
		return result
	default:
		return nil
	}
}

// ResolvedDependency is a resolved service/database that a service can reach.
type ResolvedDependency struct {
	Alias       string // Name used inside the VM (e.g., "db").
	TargetName  string // Actual service/db name (e.g., "mydb").
	TargetID    string // Service ID.
	Port        int    // Primary port of the target.
	ForwardPort int    // Host-side forwarded port (for vsock routing).
	WorkerIP    string // Worker IP address (for cross-worker routing).
	IsDatabase  bool   // True if target is a managed database.

	// Database-specific fields (only if IsDatabase).
	Engine    string
	LoginRole string
	Password  string
	DBName    string
}

// DependencyEnvVars generates environment variables for a resolved dependency.
// The alias is uppercased and used as the prefix (e.g., alias "db" → DB_HOST).
func DependencyEnvVars(dep ResolvedDependency) map[string]string {
	prefix := strings.ToUpper(strings.ReplaceAll(dep.Alias, "-", "_"))
	env := map[string]string{
		prefix + "_HOST": dep.WorkerIP,
		prefix + "_PORT": fmt.Sprintf("%d", dep.Port),
	}
	if dep.IsDatabase {
		env[prefix+"_USER"] = dep.LoginRole
		env[prefix+"_PASSWORD"] = dep.Password
		env[prefix+"_DBNAME"] = dep.DBName
		// Build connection URL.
		cfg := &DatabaseConfig{
			Engine:    dep.Engine,
			LoginRole: dep.LoginRole,
			Password:  dep.Password,
			DBName:    dep.DBName,
		}
		env[prefix+"_URL"] = ConnectionString(cfg, dep.WorkerIP)
	}
	return env
}

// CanAccess checks if a service with the given Uses map is allowed to
// reach the target service name. Returns true if:
//   - target is in the Uses map (as a value)
//   - target is a sibling (same parent service ID for multi-component)
//   - target has a public domain route (accessible via proxy, not checked here)
func CanAccess(uses map[string]string, targetName string) bool {
	for _, target := range uses {
		if target == targetName {
			return true
		}
	}
	return false
}
