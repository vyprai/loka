package loka

import (
	"strings"
	"testing"
)

func TestNormalizeUses_MapStringInterface(t *testing.T) {
	raw := map[string]interface{}{
		"db":    "mydb",
		"cache": "shared-redis",
	}
	got := NormalizeUses(raw)
	if got["db"] != "mydb" {
		t.Errorf("db = %q, want mydb", got["db"])
	}
	if got["cache"] != "shared-redis" {
		t.Errorf("cache = %q, want shared-redis", got["cache"])
	}
}

func TestNormalizeUses_ListInterface(t *testing.T) {
	raw := []interface{}{"mydb", "payment-svc"}
	got := NormalizeUses(raw)
	if got["mydb"] != "mydb" {
		t.Errorf("mydb = %q, want mydb (alias == target)", got["mydb"])
	}
	if got["payment-svc"] != "payment-svc" {
		t.Errorf("payment-svc = %q, want payment-svc", got["payment-svc"])
	}
}

func TestNormalizeUses_MapStringString(t *testing.T) {
	raw := map[string]string{"db": "mydb"}
	got := NormalizeUses(raw)
	if got["db"] != "mydb" {
		t.Errorf("db = %q, want mydb", got["db"])
	}
}

func TestNormalizeUses_ListString(t *testing.T) {
	raw := []string{"svc-a", "svc-b"}
	got := NormalizeUses(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got["svc-a"] != "svc-a" {
		t.Errorf("svc-a = %q, want svc-a", got["svc-a"])
	}
}

func TestNormalizeUses_Nil(t *testing.T) {
	got := NormalizeUses(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestNormalizeUses_EmptyList(t *testing.T) {
	got := NormalizeUses([]interface{}{})
	if got != nil {
		t.Errorf("expected nil for empty list, got %v", got)
	}
}

func TestNormalizeUses_UnsupportedType(t *testing.T) {
	got := NormalizeUses(42)
	if got != nil {
		t.Errorf("expected nil for unsupported type, got %v", got)
	}
}

func TestDependencyEnvVars_Service(t *testing.T) {
	dep := ResolvedDependency{
		Alias:    "cache",
		WorkerIP: "10.0.0.5",
		Port:     6379,
	}
	env := DependencyEnvVars(dep)
	if env["CACHE_HOST"] != "10.0.0.5" {
		t.Errorf("CACHE_HOST = %q, want 10.0.0.5", env["CACHE_HOST"])
	}
	if env["CACHE_PORT"] != "6379" {
		t.Errorf("CACHE_PORT = %q, want 6379", env["CACHE_PORT"])
	}
	// Non-database: no URL/USER/PASSWORD.
	if _, ok := env["CACHE_URL"]; ok {
		t.Error("unexpected CACHE_URL for non-database dependency")
	}
}

func TestDependencyEnvVars_Database(t *testing.T) {
	dep := ResolvedDependency{
		Alias:      "db",
		WorkerIP:   "10.0.0.3",
		Port:       5432,
		IsDatabase: true,
		Engine:     "postgres",
		LoginRole:  "app_login",
		Password:   "secret",
		DBName:     "appdb",
	}
	env := DependencyEnvVars(dep)
	if env["DB_HOST"] != "10.0.0.3" {
		t.Errorf("DB_HOST = %q, want 10.0.0.3", env["DB_HOST"])
	}
	if env["DB_PORT"] != "5432" {
		t.Errorf("DB_PORT = %q, want 5432", env["DB_PORT"])
	}
	if env["DB_USER"] != "app_login" {
		t.Errorf("DB_USER = %q, want app_login", env["DB_USER"])
	}
	if env["DB_PASSWORD"] != "secret" {
		t.Errorf("DB_PASSWORD = %q, want secret", env["DB_PASSWORD"])
	}
	if env["DB_URL"] == "" {
		t.Error("expected non-empty DB_URL")
	}
}

func TestDependencyEnvVars_HyphenatedAlias(t *testing.T) {
	dep := ResolvedDependency{
		Alias:    "payment-svc",
		WorkerIP: "10.0.0.9",
		Port:     8080,
	}
	env := DependencyEnvVars(dep)
	// Hyphens converted to underscores.
	if env["PAYMENT_SVC_HOST"] != "10.0.0.9" {
		t.Errorf("PAYMENT_SVC_HOST = %q, want 10.0.0.9", env["PAYMENT_SVC_HOST"])
	}
}

func TestCanAccess(t *testing.T) {
	uses := map[string]string{
		"db":    "mydb",
		"cache": "shared-redis",
	}

	if !CanAccess(uses, "mydb") {
		t.Error("expected access to mydb")
	}
	if !CanAccess(uses, "shared-redis") {
		t.Error("expected access to shared-redis")
	}
	if CanAccess(uses, "secret-svc") {
		t.Error("expected NO access to secret-svc")
	}
}

func TestDependencyEnvVars_EmptyAlias(t *testing.T) {
	dep := ResolvedDependency{Alias: "", WorkerIP: "10.0.0.1", Port: 5432}
	env := DependencyEnvVars(dep)
	// Empty alias produces "_HOST", "_PORT" — ugly but shouldn't crash.
	if _, ok := env["_HOST"]; !ok {
		t.Error("expected _HOST key for empty alias")
	}
}

func TestNormalizeUses_EmptyStringValue(t *testing.T) {
	raw := map[string]interface{}{"db": ""}
	got := NormalizeUses(raw)
	if got["db"] != "" {
		t.Errorf("db = %q, want empty string", got["db"])
	}
}

func TestNormalizeUses_NumericAndNilValues(t *testing.T) {
	raw := map[string]interface{}{"num": 123, "nil_val": nil}
	got := NormalizeUses(raw)
	if got["num"] != "123" {
		t.Errorf("num = %q, want '123'", got["num"])
	}
	if got["nil_val"] != "<nil>" {
		t.Errorf("nil_val = %q, want '<nil>'", got["nil_val"])
	}
}

func TestCanAccess_DuplicateTargets(t *testing.T) {
	uses := map[string]string{"api": "svc-a", "backend": "svc-a"}
	if !CanAccess(uses, "svc-a") {
		t.Error("expected access to svc-a via either alias")
	}
}

func TestCanAccess_EmptyUses(t *testing.T) {
	if CanAccess(nil, "anything") {
		t.Error("expected no access with nil uses")
	}
	if CanAccess(map[string]string{}, "anything") {
		t.Error("expected no access with empty uses")
	}
}

func TestNormalizeUses_InvalidAliasChars(t *testing.T) {
	raw := map[string]interface{}{
		"valid_alias":   "target1",
		"invalid!alias": "target2", // ! not allowed
		"also/bad":      "target3", // / not allowed
	}
	got := NormalizeUses(raw)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got["valid_alias"] != "target1" {
		t.Error("valid alias should be kept")
	}
	if _, ok := got["invalid!alias"]; ok {
		t.Error("invalid alias should be filtered out")
	}
	if _, ok := got["also/bad"]; ok {
		t.Error("alias with / should be filtered out")
	}
}

func TestDependencyEnvVars_VeryLongAlias(t *testing.T) {
	longAlias := "a_very_long_service_name_that_goes_on_and_on_and_on_for_many_characters"
	dep := ResolvedDependency{Alias: longAlias, WorkerIP: "10.0.0.1", Port: 80}
	env := DependencyEnvVars(dep)
	// Should produce very long env var key but not crash.
	key := strings.ToUpper(strings.ReplaceAll(longAlias, "-", "_")) + "_HOST"
	if env[key] != "10.0.0.1" {
		t.Errorf("expected %s=10.0.0.1, got %q", key, env[key])
	}
}
