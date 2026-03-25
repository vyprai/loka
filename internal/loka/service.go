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
	Subdomain    string `json:"subdomain,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	Port         int    `json:"port"`
	Protocol     string `json:"protocol,omitempty"` // "http" default, "grpc", "tcp"
}

// VolumeMount describes a storage volume attached to a service.
type VolumeMount struct {
	Path         string `json:"path"`
	Provider     string `json:"provider"`              // "volume", "s3", "gcs", "azure"
	Name         string `json:"name,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Prefix       string `json:"prefix,omitempty"`      // Object key prefix within bucket.
	Region       string `json:"region,omitempty"`
	Credentials  string `json:"credentials,omitempty"` // ${secret.name}
	Access       string `json:"access,omitempty"`      // "readwrite" (default) or "readonly"
	Mode         string `json:"mode,omitempty"`         // "fuse" (default, real-time) or "block" (periodic sync)
	SyncInterval int    `json:"sync_interval,omitempty"` // Seconds between syncs for block mode (default 30).
}

// IsReadOnly returns true if the mount access is "readonly".
func (v *VolumeMount) IsReadOnly() bool {
	return v.Access == "readonly"
}

// EffectiveMode returns the mount mode, defaulting to "fuse".
func (v *VolumeMount) EffectiveMode() string {
	if v.Mode == "block" {
		return "block"
	}
	return "fuse"
}

// EffectiveSyncInterval returns the sync interval for block mode, defaulting to 30s.
func (v *VolumeMount) EffectiveSyncInterval() int {
	if v.SyncInterval > 0 {
		return v.SyncInterval
	}
	return 30
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
	Mounts         []VolumeMount
	Autoscale      *AutoscaleConfig
	SnapshotID       string
	AppSnapshotMem   string // Objstore key for app-level memory snapshot.
	AppSnapshotState string // Objstore key for app-level vmstate snapshot.
	ForwardPort      int    // Local TCP port that tunnels to VM service port via vsock.
	GuestIP          string // VM guest IP for direct TCP routing (TAP networking).
	Ready          bool
	StatusMessage  string
	LastActivity   time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
