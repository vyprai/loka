package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// ControlPlaneConfig.Defaults()
// ---------------------------------------------------------------------------

func TestControlPlaneConfigDefaults(t *testing.T) {
	var c ControlPlaneConfig
	c.Defaults()

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"Mode", c.Mode, "single"},
		{"ListenAddr", c.ListenAddr, ":6840"},
		{"GRPCAddr", c.GRPCAddr, ":6841"},
		{"Database.Driver", c.Database.Driver, "sqlite"},
		{"Database.DSN", c.Database.DSN, "loka.db"},
		{"Coordinator.Type", c.Coordinator.Type, "local"},
		{"ObjectStore.Type", c.ObjectStore.Type, "local"},
		{"ObjectStore.Path", c.ObjectStore.Path, "/tmp/loka-data/artifacts"},
		{"Scheduler.Strategy", c.Scheduler.Strategy, "spread"},
	}

	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestControlPlaneConfigDefaultsPreservesSetValues(t *testing.T) {
	c := ControlPlaneConfig{
		Mode:       "ha",
		ListenAddr: ":9090",
		GRPCAddr:   ":9091",
		Database: DatabaseConfig{
			Driver: "postgres",
			DSN:    "postgres://localhost/loka",
		},
		Coordinator: CoordinatorConfig{
			Type: "raft",
		},
		ObjectStore: ObjectStoreConfig{
			Type:   "s3",
			Bucket: "my-bucket",
		},
		Scheduler: SchedulerConfig{
			Strategy: "binpack",
		},
	}
	c.Defaults()

	if c.Mode != "ha" {
		t.Errorf("Mode = %q, want ha", c.Mode)
	}
	if c.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090", c.ListenAddr)
	}
	if c.GRPCAddr != ":9091" {
		t.Errorf("GRPCAddr = %q, want :9091", c.GRPCAddr)
	}
	if c.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q, want postgres", c.Database.Driver)
	}
	if c.Database.DSN != "postgres://localhost/loka" {
		t.Errorf("Database.DSN = %q, want postgres://localhost/loka", c.Database.DSN)
	}
	if c.Coordinator.Type != "raft" {
		t.Errorf("Coordinator.Type = %q, want raft", c.Coordinator.Type)
	}
	if c.ObjectStore.Type != "s3" {
		t.Errorf("ObjectStore.Type = %q, want s3", c.ObjectStore.Type)
	}
	if c.Scheduler.Strategy != "binpack" {
		t.Errorf("Scheduler.Strategy = %q, want binpack", c.Scheduler.Strategy)
	}
}

func TestControlPlaneConfigDefaultsDSNOnlyForSQLite(t *testing.T) {
	c := ControlPlaneConfig{
		Database: DatabaseConfig{
			Driver: "postgres",
			// DSN intentionally empty.
		},
	}
	c.Defaults()

	// For non-sqlite drivers, the DSN default should NOT be "loka.db".
	if c.Database.DSN == "loka.db" {
		t.Error("DSN should not default to loka.db for postgres driver")
	}
}

func TestControlPlaneConfigDefaultsObjectStorePathOnlyForLocal(t *testing.T) {
	c := ControlPlaneConfig{
		ObjectStore: ObjectStoreConfig{
			Type: "s3",
		},
	}
	c.Defaults()

	if c.ObjectStore.Path == "/tmp/loka-data/artifacts" {
		t.Error("ObjectStore.Path should not be set for s3 type")
	}
}

// ---------------------------------------------------------------------------
// WorkerConfig.Defaults()
// ---------------------------------------------------------------------------

func TestWorkerConfigDefaults(t *testing.T) {
	var c WorkerConfig
	c.Defaults()

	if c.ControlPlane.Address != "localhost:6841" {
		t.Errorf("ControlPlane.Address = %q, want localhost:6841", c.ControlPlane.Address)
	}
	if c.DataDir != "/var/loka/worker" {
		t.Errorf("DataDir = %q, want /var/loka/worker", c.DataDir)
	}
	if c.Provider != "local" {
		t.Errorf("Provider = %q, want local", c.Provider)
	}
}

func TestWorkerConfigDefaultsPreservesSetValues(t *testing.T) {
	c := WorkerConfig{
		ControlPlane: WorkerCPConfig{
			Address: "cp.example.com:6841",
		},
		DataDir:  "/opt/loka",
		Provider: "aws",
		Token:    "loka_abc123",
		Labels:   map[string]string{"zone": "us-east-1a"},
	}
	c.Defaults()

	if c.ControlPlane.Address != "cp.example.com:6841" {
		t.Errorf("Address = %q, want cp.example.com:6841", c.ControlPlane.Address)
	}
	if c.DataDir != "/opt/loka" {
		t.Errorf("DataDir = %q, want /opt/loka", c.DataDir)
	}
	if c.Provider != "aws" {
		t.Errorf("Provider = %q, want aws", c.Provider)
	}
	if c.Token != "loka_abc123" {
		t.Errorf("Token = %q, want loka_abc123", c.Token)
	}
}

// ---------------------------------------------------------------------------
// Load from YAML
// ---------------------------------------------------------------------------

func TestLoadControlPlaneConfig(t *testing.T) {
	yaml := `
mode: ha
listen_addr: ":8080"
grpc_addr: ":8081"
database:
  driver: postgres
  dsn: "postgres://localhost/lokadb"
scheduler:
  strategy: binpack
auth:
  api_key: "secret-key"
logging:
  format: json
  level: debug
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	var c ControlPlaneConfig
	if err := Load(path, &c); err != nil {
		t.Fatal(err)
	}

	if c.Mode != "ha" {
		t.Errorf("Mode = %q, want ha", c.Mode)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
	}
	if c.GRPCAddr != ":8081" {
		t.Errorf("GRPCAddr = %q, want :8081", c.GRPCAddr)
	}
	if c.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q, want postgres", c.Database.Driver)
	}
	if c.Scheduler.Strategy != "binpack" {
		t.Errorf("Scheduler.Strategy = %q, want binpack", c.Scheduler.Strategy)
	}
	if c.Auth.APIKey != "secret-key" {
		t.Errorf("Auth.APIKey = %q, want secret-key", c.Auth.APIKey)
	}
	if c.Logging.Format != "json" {
		t.Errorf("Logging.Format = %q, want json", c.Logging.Format)
	}
	if c.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug", c.Logging.Level)
	}
}

func TestLoadWorkerConfig(t *testing.T) {
	yaml := `
control_plane:
  address: "cp.example.com:6841"
  tls: true
  insecure: false
data_dir: "/opt/loka"
provider: aws
token: "loka_xyz"
labels:
  region: us-east-1
  tier: gpu
`
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	var c WorkerConfig
	if err := Load(path, &c); err != nil {
		t.Fatal(err)
	}

	if c.ControlPlane.Address != "cp.example.com:6841" {
		t.Errorf("Address = %q", c.ControlPlane.Address)
	}
	if !c.ControlPlane.TLS {
		t.Error("TLS should be true")
	}
	if c.Provider != "aws" {
		t.Errorf("Provider = %q, want aws", c.Provider)
	}
	if c.Token != "loka_xyz" {
		t.Errorf("Token = %q", c.Token)
	}
	if c.Labels["region"] != "us-east-1" {
		t.Errorf("Labels[region] = %q", c.Labels["region"])
	}
	if c.Labels["tier"] != "gpu" {
		t.Errorf("Labels[tier] = %q", c.Labels["tier"])
	}
}

func TestLoadNonexistentFile(t *testing.T) {
	var c ControlPlaneConfig
	err := Load("/nonexistent/path.yaml", &c)
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	var c ControlPlaneConfig
	err := Load(path, &c)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	var c ControlPlaneConfig
	// Loading an empty file should not error (all fields zero-valued).
	if err := Load(path, &c); err != nil {
		t.Fatal(err)
	}

	// After Defaults(), should get sensible values.
	c.Defaults()
	if c.Mode != "single" {
		t.Errorf("Mode = %q, want single", c.Mode)
	}
}
