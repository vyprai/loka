package config

// WorkerConfig is the configuration for the loka-worker agent.
type WorkerConfig struct {
	ControlPlane WorkerCPConfig `yaml:"control_plane"`
	DataDir      string         `yaml:"data_dir"`   // Local data directory for overlays, caches.
	Provider     string         `yaml:"provider"`    // Provider name (e.g., "aws", "local", "selfmanaged").
	Token        string         `yaml:"token"`       // Registration token.
	Labels       map[string]string `yaml:"labels"`
	TLS          TLSConfig      `yaml:"tls"`
}

// WorkerCPConfig specifies how the worker connects to the control plane.
type WorkerCPConfig struct {
	Address  string `yaml:"address"`  // Control plane gRPC address.
	TLS      bool   `yaml:"tls"`      // Use TLS to connect.
	CACert   string `yaml:"ca_cert"`  // Path to CA certificate for verifying the server.
	Insecure bool   `yaml:"insecure"` // Skip TLS verification (not recommended).
}

// Defaults fills in default values for unset fields.
func (c *WorkerConfig) Defaults() {
	if c.ControlPlane.Address == "" {
		c.ControlPlane.Address = "localhost:6841"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/loka/worker"
	}
	if c.Provider == "" {
		c.Provider = "local"
	}
}
