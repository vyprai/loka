package ovh

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/provider"
)

// Config holds OVH provider configuration.
type Config struct {
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
	Region            string
}

// Provider provisions workers on OVH bare-metal servers.
type Provider struct {
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *Provider {
	return &Provider{cfg: cfg, logger: logger}
}

func (p *Provider) Name() string { return "ovh" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "kimsufi-ks-le-1" // Budget bare-metal.
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning OVH workers", "count", opts.Count, "type", opts.InstanceType)

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		workers = append(workers, &provider.WorkerInfo{
			ID:       "ovh-" + uuid.New().String()[:8],
			Provider: "ovh",
			Region:   p.cfg.Region,
			Status:   provider.WorkerInfraProvisioning,
		})
	}
	return workers, fmt.Errorf("OVH provisioning not yet implemented")
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	return fmt.Errorf("OVH deprovisioning not yet implemented")
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) { return nil, nil }

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	return provider.WorkerInfraRunning, nil
}

var _ provider.Provider = (*Provider)(nil)
