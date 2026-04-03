package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

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
	CredentialsJSON string // Service account JSON key (optional, uses ADC if empty).
}

// Provider provisions workers on GCP Compute Engine.
// Uses instances with nested virtualization enabled for KVM.
type Provider struct {
	cfg    Config
	svc    *compute.Service
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Provider, error) {
	if cfg.Zone == "" {
		cfg.Zone = "us-central1-a"
	}
	if cfg.ImageFamily == "" {
		cfg.ImageFamily = "ubuntu-2204-lts"
	}

	var opts []option.ClientOption
	if cfg.CredentialsJSON != "" {
		opts = append(opts, option.WithCredentialsJSON([]byte(cfg.CredentialsJSON)))
	}

	svc, err := compute.NewService(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("create compute service: %w", err)
	}

	return &Provider{cfg: cfg, svc: svc, logger: logger}, nil
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

	userdata := provider.GenerateCloudInit(provider.BootstrapConfig{
		ControlPlaneAddr: opts.UserData,
		Token:            opts.Labels["_token"],
		Provider:         "gcp",
		Labels:           opts.Labels,
	})

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		name := fmt.Sprintf("loka-worker-%d-%d", time.Now().Unix(), i)

		instance := &compute.Instance{
			Name:        name,
			MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, opts.InstanceType),
			Disks: []*compute.AttachedDisk{{
				Boot:       true,
				AutoDelete: true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: fmt.Sprintf("projects/ubuntu-os-cloud/global/images/family/%s", p.cfg.ImageFamily),
					DiskSizeGb:  100,
				},
			}},
			NetworkInterfaces: []*compute.NetworkInterface{{
				Network:    p.cfg.Network,
				Subnetwork: p.cfg.Subnet,
				AccessConfigs: []*compute.AccessConfig{{
					Name: "External NAT",
					Type: "ONE_TO_ONE_NAT",
				}},
			}},
			Metadata: &compute.Metadata{
				Items: []*compute.MetadataItems{{
					Key:   "user-data",
					Value: &userdata,
				}},
			},
			Labels: map[string]string{"loka-managed": "true"},
			// Enable nested virtualization for KVM support.
			AdvancedMachineFeatures: &compute.AdvancedMachineFeatures{
				EnableNestedVirtualization: true,
			},
		}

		if p.cfg.ServiceAccount != "" {
			instance.ServiceAccounts = []*compute.ServiceAccount{{
				Email:  p.cfg.ServiceAccount,
				Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
			}}
		}

		op, err := p.svc.Instances.Insert(p.cfg.ProjectID, zone, instance).Context(ctx).Do()
		if err != nil {
			return workers, fmt.Errorf("create instance %s: %w", name, err)
		}

		// Poll until operation completes.
		for op.Status != "DONE" {
			time.Sleep(3 * time.Second)
			op, err = p.svc.ZoneOperations.Get(p.cfg.ProjectID, zone, op.Name).Context(ctx).Do()
			if err != nil {
				break
			}
		}

		// Fetch instance details.
		inst, err := p.svc.Instances.Get(p.cfg.ProjectID, zone, name).Context(ctx).Do()
		if err != nil {
			workers = append(workers, &provider.WorkerInfo{
				ID: name, Provider: "gcp", Zone: zone,
				Status: provider.WorkerInfraProvisioning,
			})
			continue
		}

		w := &provider.WorkerInfo{
			ID:       name,
			Provider: "gcp",
			Zone:     zone,
			Region:   zone[:strings.LastIndex(zone, "-")],
			Status:   mapGCPStatus(inst.Status),
			Metadata: map[string]string{"machine_type": opts.InstanceType},
		}
		for _, ni := range inst.NetworkInterfaces {
			w.InternalIP = ni.NetworkIP
			for _, ac := range ni.AccessConfigs {
				w.ExternalIP = ac.NatIP
			}
		}
		workers = append(workers, w)
	}

	p.logger.Info("GCP workers provisioned", "count", len(workers))
	return workers, nil
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	p.logger.Info("deprovisioning GCP worker", "instance", workerID)
	_, err := p.svc.Instances.Delete(p.cfg.ProjectID, p.cfg.Zone, workerID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	result, err := p.svc.Instances.List(p.cfg.ProjectID, p.cfg.Zone).
		Filter(`labels.loka-managed="true"`).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}

	var workers []*provider.WorkerInfo
	for _, inst := range result.Items {
		w := &provider.WorkerInfo{
			ID: inst.Name, Provider: "gcp", Zone: p.cfg.Zone,
			Status: mapGCPStatus(inst.Status),
		}
		for _, ni := range inst.NetworkInterfaces {
			w.InternalIP = ni.NetworkIP
			for _, ac := range ni.AccessConfigs {
				w.ExternalIP = ac.NatIP
			}
		}
		workers = append(workers, w)
	}
	return workers, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	inst, err := p.svc.Instances.Get(p.cfg.ProjectID, p.cfg.Zone, workerID).Context(ctx).Do()
	if err != nil {
		return provider.WorkerInfraError, err
	}
	return mapGCPStatus(inst.Status), nil
}

func mapGCPStatus(status string) provider.WorkerInfraStatus {
	switch status {
	case "PROVISIONING", "STAGING":
		return provider.WorkerInfraProvisioning
	case "RUNNING":
		return provider.WorkerInfraRunning
	case "STOPPING", "SUSPENDING":
		return provider.WorkerInfraTerminating
	case "TERMINATED", "STOPPED", "SUSPENDED":
		return provider.WorkerInfraTerminated
	default:
		return provider.WorkerInfraError
	}
}

var _ provider.Provider = (*Provider)(nil)
