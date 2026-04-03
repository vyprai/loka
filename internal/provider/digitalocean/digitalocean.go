package digitalocean

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/digitalocean/godo"

	"github.com/vyprai/loka/internal/provider"
)

// Config holds DigitalOcean provider configuration.
type Config struct {
	Token  string // API token.
	Region string // e.g., "nyc1", "sfo3"
}

// Provider provisions workers as DigitalOcean Droplets.
type Provider struct {
	cfg    Config
	client *godo.Client
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Provider, error) {
	if cfg.Region == "" {
		cfg.Region = "nyc1"
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("DigitalOcean API token is required")
	}
	return &Provider{
		cfg:    cfg,
		client: godo.NewFromToken(cfg.Token),
		logger: logger,
	}, nil
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

	p.logger.Info("provisioning DigitalOcean workers",
		"count", opts.Count,
		"size", opts.InstanceType,
		"region", region,
	)

	userdata := provider.GenerateCloudInit(provider.BootstrapConfig{
		ControlPlaneAddr: opts.UserData,
		Token:            opts.Labels["_token"],
		Provider:         "digitalocean",
		Labels:           opts.Labels,
	})

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		name := fmt.Sprintf("loka-worker-%d-%d", time.Now().Unix(), i)

		createReq := &godo.DropletCreateRequest{
			Name:     name,
			Region:   region,
			Size:     opts.InstanceType,
			Image:    godo.DropletCreateImage{Slug: "ubuntu-22-04-x64"},
			UserData: userdata,
			Tags:     []string{"loka-managed"},
		}

		droplet, _, err := p.client.Droplets.Create(ctx, createReq)
		if err != nil {
			return workers, fmt.Errorf("create droplet %s: %w", name, err)
		}

		w := dropletToWorkerInfo(droplet)
		workers = append(workers, w)
	}

	p.logger.Info("DigitalOcean workers provisioned", "count", len(workers))
	return workers, nil
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	p.logger.Info("deprovisioning DigitalOcean worker", "droplet", workerID)
	id, err := strconv.Atoi(workerID)
	if err != nil {
		return fmt.Errorf("invalid droplet ID: %w", err)
	}
	_, err = p.client.Droplets.Delete(ctx, id)
	return err
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	droplets, _, err := p.client.Droplets.ListByTag(ctx, "loka-managed", &godo.ListOptions{PerPage: 200})
	if err != nil {
		return nil, fmt.Errorf("list droplets: %w", err)
	}

	var workers []*provider.WorkerInfo
	for i := range droplets {
		workers = append(workers, dropletToWorkerInfo(&droplets[i]))
	}
	return workers, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	id, err := strconv.Atoi(workerID)
	if err != nil {
		return provider.WorkerInfraError, fmt.Errorf("invalid droplet ID: %w", err)
	}
	droplet, _, err := p.client.Droplets.Get(ctx, id)
	if err != nil {
		return provider.WorkerInfraError, err
	}
	return mapDOStatus(droplet.Status), nil
}

func dropletToWorkerInfo(d *godo.Droplet) *provider.WorkerInfo {
	w := &provider.WorkerInfo{
		ID:       strconv.Itoa(d.ID),
		Provider: "digitalocean",
		Region:   d.Region.Slug,
		Status:   mapDOStatus(d.Status),
		Metadata: map[string]string{"size": d.Size.Slug},
	}
	pub, _ := d.PublicIPv4()
	priv, _ := d.PrivateIPv4()
	w.ExternalIP = pub
	w.InternalIP = priv
	return w
}

func mapDOStatus(status string) provider.WorkerInfraStatus {
	switch status {
	case "new":
		return provider.WorkerInfraProvisioning
	case "active":
		return provider.WorkerInfraRunning
	case "off", "archive":
		return provider.WorkerInfraTerminated
	default:
		return provider.WorkerInfraError
	}
}

var _ provider.Provider = (*Provider)(nil)
