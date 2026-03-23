package azure

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/provider"
)

// Config holds Azure provider configuration.
type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	VNetName       string
	SubnetName     string
	ImageReference string // e.g., "Canonical:0001-com-ubuntu-server-jammy:22_04-lts:latest"
}

// Provider provisions workers on Azure VMs with nested virtualization.
type Provider struct {
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *Provider {
	return &Provider{cfg: cfg, logger: logger}
}

func (p *Provider) Name() string { return "azure" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "Standard_D8s_v3" // Supports nested virtualization.
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning Azure workers", "count", opts.Count, "size", opts.InstanceType)

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		workers = append(workers, &provider.WorkerInfo{
			ID:       "azure-" + uuid.New().String()[:8],
			Provider: "azure",
			Region:   p.cfg.Location,
			Status:   provider.WorkerInfraProvisioning,
		})
	}
	return workers, fmt.Errorf("Azure provisioning not yet implemented")
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	return fmt.Errorf("Azure deprovisioning not yet implemented")
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) { return nil, nil }

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	return provider.WorkerInfraRunning, nil
}

var _ provider.Provider = (*Provider)(nil)
