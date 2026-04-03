package ovh

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	ovhclient "github.com/ovh/go-ovh/ovh"

	"github.com/vyprai/loka/internal/provider"
)

// Config holds OVH provider configuration.
type Config struct {
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
	Region            string // OVH cloud region (e.g., "GRA11", "SBG5").
	ProjectID         string // OVH Public Cloud project ID.
}

// ovhInstance represents an OVH cloud instance from the API.
type ovhInstance struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ACTIVE, BUILD, ERROR, DELETED, etc.
	Region string `json:"region"`
	IPs    []struct {
		IP      string `json:"ip"`
		Type    string `json:"type"` // "public" or "private"
		Version int    `json:"version"`
	} `json:"ipAddresses"`
}

// Provider provisions workers on OVH Public Cloud.
type Provider struct {
	cfg    Config
	client *ovhclient.Client
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Provider, error) {
	if cfg.Region == "" {
		cfg.Region = "GRA11"
	}
	if cfg.ApplicationKey == "" {
		return nil, fmt.Errorf("OVH application key is required")
	}

	client, err := ovhclient.NewClient(
		ovhclient.OvhEU,
		cfg.ApplicationKey,
		cfg.ApplicationSecret,
		cfg.ConsumerKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create OVH client: %w", err)
	}

	return &Provider{cfg: cfg, client: client, logger: logger}, nil
}

func (p *Provider) Name() string { return "ovh" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "b2-30" // OVH cloud instance flavor.
	}
	region := opts.Region
	if region == "" {
		region = p.cfg.Region
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning OVH workers",
		"count", opts.Count,
		"flavor", opts.InstanceType,
		"region", region,
	)

	userdata := provider.GenerateCloudInit(provider.BootstrapConfig{
		ControlPlaneAddr: opts.UserData,
		Token:            opts.Labels["_token"],
		Provider:         "ovh",
		Labels:           opts.Labels,
	})

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		name := fmt.Sprintf("loka-worker-%d-%d", time.Now().Unix(), i)

		reqBody := map[string]any{
			"name":       name,
			"flavorId":   opts.InstanceType,
			"imageId":    "Ubuntu 22.04", // OVH image name.
			"region":     region,
			"userData":   userdata,
		}

		var result ovhInstance
		endpoint := fmt.Sprintf("/cloud/project/%s/instance", p.cfg.ProjectID)
		err := p.client.Post(endpoint, reqBody, &result)
		if err != nil {
			return workers, fmt.Errorf("create instance %s: %w", name, err)
		}

		workers = append(workers, ovhToWorkerInfo(&result))
	}

	p.logger.Info("OVH workers provisioned", "count", len(workers))
	return workers, nil
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	p.logger.Info("deprovisioning OVH worker", "instance", workerID)
	endpoint := fmt.Sprintf("/cloud/project/%s/instance/%s", p.cfg.ProjectID, workerID)
	return p.client.Delete(endpoint, nil)
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	endpoint := fmt.Sprintf("/cloud/project/%s/instance", p.cfg.ProjectID)
	var instances []ovhInstance
	if err := p.client.Get(endpoint, &instances); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}

	var workers []*provider.WorkerInfo
	for i := range instances {
		if strings.HasPrefix(instances[i].Name, "loka-") {
			workers = append(workers, ovhToWorkerInfo(&instances[i]))
		}
	}
	return workers, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	endpoint := fmt.Sprintf("/cloud/project/%s/instance/%s", p.cfg.ProjectID, workerID)
	var inst ovhInstance
	if err := p.client.Get(endpoint, &inst); err != nil {
		return provider.WorkerInfraError, err
	}
	return mapOVHStatus(inst.Status), nil
}

func ovhToWorkerInfo(inst *ovhInstance) *provider.WorkerInfo {
	w := &provider.WorkerInfo{
		ID:       inst.ID,
		Provider: "ovh",
		Region:   inst.Region,
		Status:   mapOVHStatus(inst.Status),
		Metadata: map[string]string{},
	}
	for _, ip := range inst.IPs {
		if ip.Version == 4 {
			if ip.Type == "public" {
				w.ExternalIP = ip.IP
			} else {
				w.InternalIP = ip.IP
			}
		}
	}
	return w
}

func mapOVHStatus(status string) provider.WorkerInfraStatus {
	switch strings.ToUpper(status) {
	case "BUILD":
		return provider.WorkerInfraProvisioning
	case "ACTIVE":
		return provider.WorkerInfraRunning
	case "DELETED", "STOPPED", "SHUTOFF":
		return provider.WorkerInfraTerminated
	case "ERROR":
		return provider.WorkerInfraError
	default:
		return provider.WorkerInfraProvisioning
	}
}

var _ provider.Provider = (*Provider)(nil)
