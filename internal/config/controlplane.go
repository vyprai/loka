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
	Metrics     MetricsConfig     `yaml:"metrics"`
	LogStore    LogStoreConfig    `yaml:"log_store"`
}

// LogStoreConfig configures the centralized log store.
type LogStoreConfig struct {
	Enabled        bool   `yaml:"enabled"`         // Enable centralized log store (default true).
	DataDir        string `yaml:"data_dir"`          // BadgerDB directory for logs.
	Retention      string `yaml:"retention"`         // TTL for log entries (default "168h" / 7 days).
	ScrapeInterval string `yaml:"scrape_interval"`   // How often to pull from workers (default "5s").
	MaxDiskBytes   int64  `yaml:"max_disk_bytes"`    // Max disk usage for logs (default 10GB, 0=unlimited).
}

// MetricsConfig configures the built-in metrics TSDB.
type MetricsConfig struct {
	Enabled         bool             `yaml:"enabled"`          // Enable built-in metrics (default true).
	DataDir         string           `yaml:"data_dir"`          // BadgerDB storage directory.
	ScrapeInterval  string           `yaml:"scrape_interval"`   // Target scrape interval (default "15s").
	CollectInterval string           `yaml:"collect_interval"`  // CP-level collection interval (default "15s").
	Retention       MetricsRetention `yaml:"retention"`
	MaxDiskBytes    int64            `yaml:"max_disk_bytes"`    // Max disk usage for metrics (default 10GB, 0=unlimited).
}

// MetricsRetention configures retention tiers for metrics data.
type MetricsRetention struct {
	Raw         string `yaml:"raw"`          // Full-resolution retention (default "48h").
	Downsampled string `yaml:"downsampled"`  // 1-minute aggregate retention (default "168h").
	Meta        string `yaml:"meta"`         // Index-only retention (default "720h").
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

// DomainConfig configures the optional domain-based reverse proxy.
type DomainConfig struct {
	Enabled    bool   `yaml:"enabled"`     // Enable domain forwarding.
	ListenAddr string `yaml:"listen_addr"` // Separate listener for proxied traffic (default ":6843")
	DNSAddr    string `yaml:"dns_addr"`    // DNS server listen address (default ":5453")
	DNSEnabled bool   `yaml:"dns_enabled"` // Enable built-in DNS server.
	DNSDomain  string `yaml:"dns_domain"`  // TLD for built-in DNS resolution (default "loka")
}

// AuthConfig configures API authentication.
type AuthConfig struct {
	APIKey        string `yaml:"api_key"`        // If set, all API requests must include this key.
	AdminKey      string `yaml:"admin_key"`       // Separate key for admin endpoints (optional).
	EncryptionKey string `yaml:"encryption_key"`  // Key for encrypting credentials at rest (optional).
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
	Type      string `yaml:"type"`       // "local", "s3", "gcs", "azure"
	Path      string `yaml:"path"`       // Local/embedded filesystem path.
	Bucket    string `yaml:"bucket"`     // S3/GCS bucket name or Azure container name.
	Region    string `yaml:"region"`     // S3/Azure region.
	Endpoint  string `yaml:"endpoint"`   // Custom S3 endpoint (for MinIO, R2, etc).
	AccessKey string `yaml:"access_key"` // S3 static access key (MinIO, R2, etc).
	SecretKey string `yaml:"secret_key"` // S3 static secret key.
	Account   string `yaml:"account"`    // Azure storage account name.
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

	// Metrics defaults.
	if !c.Metrics.Enabled && c.Metrics.DataDir == "" && c.Metrics.ScrapeInterval == "" {
		// Not explicitly configured — enable by default.
		c.Metrics.Enabled = true
	}
	if c.Metrics.DataDir == "" {
		c.Metrics.DataDir = c.DataDir + "/metrics"
	}
	if c.Metrics.ScrapeInterval == "" {
		c.Metrics.ScrapeInterval = "15s"
	}
	if c.Metrics.CollectInterval == "" {
		c.Metrics.CollectInterval = "15s"
	}
	if c.Metrics.Retention.Raw == "" {
		c.Metrics.Retention.Raw = "48h"
	}
	if c.Metrics.Retention.Downsampled == "" {
		c.Metrics.Retention.Downsampled = "168h"
	}
	if c.Metrics.Retention.Meta == "" {
		c.Metrics.Retention.Meta = "720h"
	}
	if c.Metrics.MaxDiskBytes == 0 {
		c.Metrics.MaxDiskBytes = 10 * 1024 * 1024 * 1024 // 10GB
	}

	// Log store defaults.
	if !c.LogStore.Enabled && c.LogStore.DataDir == "" && c.LogStore.ScrapeInterval == "" {
		c.LogStore.Enabled = true
	}
	if c.LogStore.DataDir == "" {
		c.LogStore.DataDir = c.DataDir + "/logs"
	}
	if c.LogStore.Retention == "" {
		c.LogStore.Retention = "168h"
	}
	if c.LogStore.ScrapeInterval == "" {
		c.LogStore.ScrapeInterval = "5s"
	}
	if c.LogStore.MaxDiskBytes == 0 {
		c.LogStore.MaxDiskBytes = 10 * 1024 * 1024 * 1024 // 10GB
	}

	// Domain proxy defaults — enable by default for local (all-in-one) setups.
	if c.Role == "" || c.Role == "all" {
		if !c.Domain.Enabled {
			c.Domain.Enabled = true
		}
	}
	if c.Domain.DNSDomain == "" {
		c.Domain.DNSDomain = "loka"
	}
	if c.Domain.ListenAddr == "" {
		c.Domain.ListenAddr = ":6843"
	}
	if c.Domain.DNSAddr == "" {
		c.Domain.DNSAddr = ":5453"
	}
}
