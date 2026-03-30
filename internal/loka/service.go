package loka

import "time"

// ServiceStatus represents the current state of a LOKA service.
type ServiceStatus string

const (
	ServiceStatusDeploying ServiceStatus = "deploying"
	ServiceStatusRunning   ServiceStatus = "running"
	ServiceStatusIdle      ServiceStatus = "idle"
	ServiceStatusWaking    ServiceStatus = "waking"
	ServiceStatusStopped   ServiceStatus = "stopped"
	ServiceStatusError     ServiceStatus = "error"
)

// ServiceRoute describes how external traffic reaches a service.
type ServiceRoute struct {
	Domain    string `json:"domain,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	Port         int    `json:"port"`
	Protocol     string `json:"protocol,omitempty"` // "http" default, "grpc", "tcp"
}

// AutoscaleConfig controls horizontal scaling of a service.
type AutoscaleConfig struct {
	Min                int     `json:"min" yaml:"min"`
	Max                int     `json:"max" yaml:"max"`
	TargetConcurrency  int     `json:"target_concurrency" yaml:"target_concurrency"`
	ScaleUpThreshold   float64 `json:"scale_up_threshold" yaml:"scale_up_threshold"`
	ScaleDownThreshold float64 `json:"scale_down_threshold" yaml:"scale_down_threshold"`
	Cooldown           int     `json:"cooldown" yaml:"cooldown"`
}

// Service represents a long-running LOKA serverless service.
type Service struct {
	ID             string
	Name           string
	Status         ServiceStatus
	WorkerID       string
	ImageRef       string // Docker image reference.
	ImageID        string // Resolved image ID after pull.
	RecipeName     string
	Command        string
	Args           []string
	Env            map[string]string
	Workdir        string
	Port           int
	VCPUs          int
	MemoryMB       int
	Routes         []ServiceRoute
	BundleKey      string
	IdleTimeout    int // Seconds of inactivity before auto-idle (0 = never).
	HealthPath     string
	HealthInterval int
	HealthTimeout  int
	HealthRetries  int
	Labels         map[string]string
	Mounts         []Volume
	Autoscale      *AutoscaleConfig
	SnapshotID       string
	AppSnapshotMem   string // Objstore key for app-level memory snapshot.
	AppSnapshotState string // Objstore key for app-level vmstate snapshot.
	ForwardPort      int    // Local TCP port that tunnels to VM service port via vsock.
	GuestIP          string // VM guest IP for direct TCP routing (TAP networking).
	DatabaseConfig  *DatabaseConfig   `json:"DatabaseConfig,omitempty"` // Non-nil = managed database instance.
	Uses            map[string]string `json:"Uses,omitempty"`          // Network ACL: alias→target service/db name.
	ParentServiceID string            `json:"ParentServiceID,omitempty"` // Links replicas/components to their primary.
	Replicas        int               `json:"Replicas,omitempty"`        // Desired instance count (on primary only).
	RelationType    string            `json:"RelationType,omitempty"`    // "replica", "component", "db_replica", "db_sentinel"
	Ready          bool
	StatusMessage  string
	LastActivity   time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Components     []ServiceComponent `json:"Components,omitempty"` // Multi-component service.
}

// ServiceComponent represents one component in a multi-component service.
// Each component runs in its own VM with its own image.
type ServiceComponent struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Port        int               `json:"port"`
	Domain      string            `json:"domain,omitempty"`     // Empty = internal only.
	WorkerID    string            `json:"worker_id,omitempty"`
	ForwardPort int               `json:"forward_port,omitempty"`
	Status      string            `json:"status"`
	BundleKey   string            `json:"bundle_key,omitempty"`
	DependsOn   []string          `json:"depends_on,omitempty"` // Component names this depends on.
}

// ValidServiceTransitions defines the allowed state transitions for a service.
var ValidServiceTransitions = map[ServiceStatus][]ServiceStatus{
	ServiceStatusDeploying: {ServiceStatusRunning, ServiceStatusStopped, ServiceStatusError},
	ServiceStatusRunning:   {ServiceStatusIdle, ServiceStatusStopped, ServiceStatusError},
	ServiceStatusIdle:      {ServiceStatusWaking, ServiceStatusStopped, ServiceStatusError},
	ServiceStatusWaking:    {ServiceStatusRunning, ServiceStatusError},
	ServiceStatusStopped:   {ServiceStatusDeploying, ServiceStatusError},
	ServiceStatusError:     {ServiceStatusDeploying, ServiceStatusStopped},
}

// CanTransitionTo checks if the service can transition to the given status.
func (s *Service) CanTransitionTo(target ServiceStatus) bool {
	allowed, ok := ValidServiceTransitions[s.Status]
	if !ok {
		return false
	}
	for _, a := range allowed {
		if a == target {
			return true
		}
	}
	return false
}
