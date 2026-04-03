package provider

import (
	"context"
	"errors"
)

// ErrNotSupported is returned when a provider doesn't support an operation.
var ErrNotSupported = errors.New("operation not supported by this provider")

// WorkerInfraStatus represents the infrastructure-level status of a worker.
type WorkerInfraStatus int

const (
	WorkerInfraProvisioning WorkerInfraStatus = iota
	WorkerInfraRunning
	WorkerInfraTerminating
	WorkerInfraTerminated
	WorkerInfraError
)

func (s WorkerInfraStatus) String() string {
	switch s {
	case WorkerInfraProvisioning:
		return "provisioning"
	case WorkerInfraRunning:
		return "running"
	case WorkerInfraTerminating:
		return "terminating"
	case WorkerInfraTerminated:
		return "terminated"
	case WorkerInfraError:
		return "error"
	default:
		return "unknown"
	}
}

// ProvisionOpts configures the provisioning of a new worker.
type ProvisionOpts struct {
	InstanceType string
	Region       string
	Zone         string
	Labels       map[string]string
	SSHKeyName   string
	UserData     string // Cloud-init / bootstrap script.
	Count        int
}

// WorkerInfo describes a provisioned or discovered worker.
type WorkerInfo struct {
	ID         string
	Provider   string
	ExternalIP string
	InternalIP string
	Region     string
	Zone       string
	Status     WorkerInfraStatus
	Metadata   map[string]string
}

// Provider provisions and manages worker infrastructure.
// Cloud providers implement full lifecycle. Self-managed only validates registration.
type Provider interface {
	// Name returns the provider identifier (e.g., "aws", "gcp", "selfmanaged").
	Name() string

	// Provision creates new worker VM(s) and returns their info.
	Provision(ctx context.Context, opts ProvisionOpts) ([]*WorkerInfo, error)

	// Deprovision terminates a worker instance.
	Deprovision(ctx context.Context, workerID string) error

	// List returns all workers managed by this provider.
	List(ctx context.Context) ([]*WorkerInfo, error)

	// WorkerStatus checks the infrastructure-level status of a worker.
	WorkerStatus(ctx context.Context, workerID string) (WorkerInfraStatus, error)
}

// SelfManagedProvider extends Provider for self-managed workers that connect inbound.
type SelfManagedProvider interface {
	Provider

	// ValidateToken validates a registration token for a self-managed worker.
	// Returns the token object if valid.
	ValidateToken(ctx context.Context, token string) (any, error)
}

// Registry holds all registered providers.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry creates a new provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register adds a provider to the registry.
func (r *Registry) Register(p Provider) {
	r.providers[p.Name()] = p
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// List returns all registered provider names.
func (r *Registry) List() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	return names
}
