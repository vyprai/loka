package digitalocean

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/provider"
)

// Config holds DigitalOcean provider configuration.
type Config struct {
	Token  string // API token.
	Region string // Default region (e.g., "nyc1").
}

// Provider provisions workers on DigitalOcean droplets.
type Provider struct {
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *Provider {
	return &Provider{cfg: cfg, logger: logger}
}

func (p *Provider) Name() string { return "digitalocean" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "s-8vcpu-16gb"
	}
	region := opts.Region
	if region == "" {
		region = p.cfg.Region
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning DigitalOcean workers", "count", opts.Count, "size", opts.InstanceType, "region", region)

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		workers = append(workers, &provider.WorkerInfo{
			ID:       "do-" + uuid.New().String()[:8],
			Provider: "digitalocean",
			Region:   region,
			Status:   provider.WorkerInfraProvisioning,
		})
	}
	return workers, fmt.Errorf("DigitalOcean provisioning not yet implemented")
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	return fmt.Errorf("DigitalOcean deprovisioning not yet implemented")
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) { return nil, nil }

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	return provider.WorkerInfraRunning, nil
}

var _ provider.Provider = (*Provider)(nil)
