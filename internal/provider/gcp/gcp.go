package gcp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/provider"
)

// Config holds GCP provider configuration.
type Config struct {
	ProjectID      string
	Zone           string
	Network        string
	Subnet         string
	ServiceAccount string
	ImageFamily    string // e.g., "ubuntu-2204-lts"
}

// Provider provisions workers on GCP Compute Engine.
// Uses instances with nested virtualization enabled for KVM.
type Provider struct {
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *Provider {
	return &Provider{cfg: cfg, logger: logger}
}

func (p *Provider) Name() string { return "gcp" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "n2-standard-8"
	}
	zone := opts.Zone
	if zone == "" {
		zone = p.cfg.Zone
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning GCP workers",
		"count", opts.Count,
		"machine_type", opts.InstanceType,
		"zone", zone,
	)

	// TODO: Replace with real GCP Compute API calls.
	// Key: enable nested virtualization via
	//   instance.AdvancedMachineFeatures.EnableNestedVirtualization = true

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		workers = append(workers, &provider.WorkerInfo{
			ID:       "gcp-" + uuid.New().String()[:8],
			Provider: "gcp",
			Region:   zone,
			Status:   provider.WorkerInfraProvisioning,
			Metadata: map[string]string{
				"machine_type": opts.InstanceType,
				"project":      p.cfg.ProjectID,
			},
		})
	}

	return workers, fmt.Errorf("GCP provisioning not yet implemented (requires GCP credentials)")
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	return fmt.Errorf("GCP deprovisioning not yet implemented")
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	return nil, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	return provider.WorkerInfraRunning, nil
}

var _ provider.Provider = (*Provider)(nil)
