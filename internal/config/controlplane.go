package config

// ControlPlaneConfig is the configuration for the lokad control plane.
type ControlPlaneConfig struct {
	Role        string            `yaml:"role"`        // "all" (CP + embedded worker) or "controlplane" (CP only)
	Mode        string            `yaml:"mode"`        // "single" or "ha"
	ListenAddr  string            `yaml:"listen_addr"` // REST API listen address (default ":6840")
	GRPCAddr    string            `yaml:"grpc_addr"`   // gRPC listen address for workers (default ":6841")
	Database    DatabaseConfig    `yaml:"database"`
	Coordinator CoordinatorConfig `yaml:"coordinator"`
	ObjectStore ObjectStoreConfig `yaml:"objectstore"`
	Scheduler   SchedulerConfig   `yaml:"scheduler"`
	Auth        AuthConfig        `yaml:"auth"`
	Logging     LoggingConfig     `yaml:"logging"`
	TLS         TLSConfig         `yaml:"tls"`
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
	Type   string `yaml:"type"`   // "local", "s3", "gcs"
	Path   string `yaml:"path"`   // Local filesystem path (type=local).
	Bucket string `yaml:"bucket"` // S3/GCS bucket name.
	Region string `yaml:"region"` // S3 region.
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
	if c.ObjectStore.Type == "" {
		c.ObjectStore.Type = "local"
	}
	if c.ObjectStore.Path == "" && c.ObjectStore.Type == "local" {
		// Use a temp dir for development; /var/loka requires root.
		c.ObjectStore.Path = "/tmp/loka-data/artifacts"
	}
	if c.Scheduler.Strategy == "" {
		c.Scheduler.Strategy = "spread"
	}
}
