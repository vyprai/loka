package config

import (
	"fmt"
	"log"
)

// ControlPlaneConfig is the configuration for the lokad control plane.
type ControlPlaneConfig struct {
	Role        string            `yaml:"role"`        // "all" (CP + embedded worker) or "controlplane" (CP only)
	Mode        string            `yaml:"mode"`        // "single" or "ha"
	ListenAddr  string            `yaml:"listen_addr"` // REST API listen address (default ":6840")
	GRPCAddr    string            `yaml:"grpc_addr"`   // gRPC listen address for workers (default ":6841")
	DataDir     string            `yaml:"data_dir"`    // Local data directory for worker files, caches, etc.
	Database    DatabaseConfig    `yaml:"database"`
	Coordinator CoordinatorConfig `yaml:"coordinator"`
	ObjectStore ObjectStoreConfig `yaml:"objectstore"`
	Scheduler   SchedulerConfig   `yaml:"scheduler"`
	Auth        AuthConfig        `yaml:"auth"`
	Logging     LoggingConfig     `yaml:"logging"`
	TLS         TLSConfig         `yaml:"tls"`
	Domain      DomainConfig      `yaml:"domain"`
	Retention   RetentionConfig   `yaml:"retention"`
}

// RetentionConfig controls garbage collection TTLs.
type RetentionConfig struct {
	SessionTTL      string `yaml:"session_ttl"`      // Default "168h" (7 days)
	CheckpointTTL   string `yaml:"checkpoint_ttl"`    // Default "168h"
	ExecutionTTL    string `yaml:"execution_ttl"`     // Default "72h" (3 days)
	TokenTTL        string `yaml:"token_ttl"`         // Default "24h"
	ImageTTL        string `yaml:"image_ttl"`         // Default "720h" (30 days)
	CleanupInterval string `yaml:"cleanup_interval"`  // Default "1h"
}

// DomainConfig configures the optional subdomain-based reverse proxy.
type DomainConfig struct {
	Enabled    bool   `yaml:"enabled"`     // Enable domain forwarding.
	BaseDomain string `yaml:"base_domain"` // e.g. "loka.example.com" → {subdomain}.loka.example.com
	ListenAddr string `yaml:"listen_addr"` // Separate listener for proxied traffic (default ":6843")
}

// AuthConfig configures API authentication.
type AuthConfig struct {
	APIKey string `yaml:"api_key"` // If set, all API requests must include this key.
}

// LoggingConfig configures logging output.
type LoggingConfig struct {
	Format string `yaml:"format"` // "text" or "json" (default "text")
	Level  string `yaml:"level"`  // "debug", "info", "warn", "error" (default "info")
}

// DatabaseConfig selects and configures the database backend.
type DatabaseConfig struct {
	Driver string `yaml:"driver"` // "sqlite" or "postgres"
	DSN    string `yaml:"dsn"`    // Connection string or file path.
}

// CoordinatorConfig selects and configures the HA coordinator.
type CoordinatorConfig struct {
	Type      string   `yaml:"type"`      // "local" or "raft"
	Address   string   `yaml:"address"`   // Raft bind address (default ":6842")
	NodeID    string   `yaml:"node_id"`   // Unique node ID (default: hostname)
	DataDir   string   `yaml:"data_dir"`  // Raft data directory (default "/var/loka/raft")
	Bootstrap bool     `yaml:"bootstrap"` // Bootstrap as first node in cluster
	Peers     []string `yaml:"peers"`     // Initial peer addresses
}

// ObjectStoreConfig selects and configures the object store.
type ObjectStoreConfig struct {
	Type     string `yaml:"type"`     // "local", "s3", "gcs", "azure"
	Path     string `yaml:"path"`     // Local/embedded filesystem path.
	Bucket   string `yaml:"bucket"`   // S3/GCS bucket name or Azure container name.
	Region   string `yaml:"region"`   // S3/Azure region.
	Endpoint string `yaml:"endpoint"` // Custom S3 endpoint (for MinIO, R2, etc).
	Account  string `yaml:"account"`  // Azure storage account name.
}

// SchedulerConfig configures the session scheduler.
type SchedulerConfig struct {
	Strategy string `yaml:"strategy"` // "binpack" or "spread" (default "spread")
}

// TLSConfig enables TLS for API and gRPC servers.
type TLSConfig struct {
	CertFile      string   `yaml:"cert"`
	KeyFile       string   `yaml:"key"`
	CACertFile    string   `yaml:"ca_cert"`
	AutoTLS       *bool    `yaml:"auto"`
	AllowInsecure bool     `yaml:"allow_insecure"`
	SANs          []string `yaml:"sans"`
}

// Validate checks the configuration for invalid or inconsistent settings.
func (c *ControlPlaneConfig) Validate() error {
	if c.Mode == "ha" {
		if c.Coordinator.Type != "raft" {
			return fmt.Errorf("HA mode requires coordinator type 'raft', got %q", c.Coordinator.Type)
		}
		if c.Coordinator.NodeID == "" {
			return fmt.Errorf("HA mode requires explicit node_id")
		}
		if c.Coordinator.Bootstrap && len(c.Coordinator.Peers) > 0 {
			log.Println("WARNING: bootstrap=true with peers set — ensure only one node bootstraps")
		}
	}
	return nil
}

// Defaults fills in default values for unset fields.
func (c *ControlPlaneConfig) Defaults() {
	if c.Role == "" {
		c.Role = "all"
	}
	if c.Mode == "" {
		c.Mode = "single"
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":6840"
	}
	if c.GRPCAddr == "" {
		c.GRPCAddr = ":6841"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.DSN == "" && c.Database.Driver == "sqlite" {
		c.Database.DSN = "loka.db"
	}
	if c.Coordinator.Type == "" {
		c.Coordinator.Type = "local"
	}
	if c.DataDir == "" {
		c.DataDir = "/tmp/loka-data"
	}
	if c.ObjectStore.Type == "" {
		c.ObjectStore.Type = "local"
	}
	if c.ObjectStore.Path == "" && c.ObjectStore.Type == "local" {
		c.ObjectStore.Path = c.DataDir + "/objstore"
	}
	if c.Scheduler.Strategy == "" {
		c.Scheduler.Strategy = "spread"
	}
	if c.Retention.SessionTTL == "" {
		c.Retention.SessionTTL = "168h"
	}
	if c.Retention.CheckpointTTL == "" {
		c.Retention.CheckpointTTL = "168h"
	}
	if c.Retention.ExecutionTTL == "" {
		c.Retention.ExecutionTTL = "72h"
	}
	if c.Retention.TokenTTL == "" {
		c.Retention.TokenTTL = "24h"
	}
	if c.Retention.ImageTTL == "" {
		c.Retention.ImageTTL = "720h"
	}
	if c.Retention.CleanupInterval == "" {
		c.Retention.CleanupInterval = "1h"
	}
}
