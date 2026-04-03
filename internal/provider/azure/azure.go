package azure

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"

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

// Provider provisions workers on Azure VMs.
type Provider struct {
	cfg    Config
	vms    *armcompute.VirtualMachinesClient
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Provider, error) {
	if cfg.Location == "" {
		cfg.Location = "eastus"
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credentials: %w", err)
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create VM client: %w", err)
	}

	return &Provider{cfg: cfg, vms: vmClient, logger: logger}, nil
}

func (p *Provider) Name() string { return "azure" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "Standard_D8s_v3"
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning Azure workers",
		"count", opts.Count,
		"vm_size", opts.InstanceType,
		"location", p.cfg.Location,
	)

	userdata := provider.GenerateCloudInit(provider.BootstrapConfig{
		ControlPlaneAddr: opts.UserData,
		Token:            opts.Labels["_token"],
		Provider:         "azure",
		Labels:           opts.Labels,
	})

	imageRef := parseImageReference(p.cfg.ImageReference)

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		vmName := fmt.Sprintf("loka-worker-%d-%d", time.Now().Unix(), i)

		poller, err := p.vms.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, vmName,
			armcompute.VirtualMachine{
				Location: to.Ptr(p.cfg.Location),
				Tags:     map[string]*string{"loka-managed": to.Ptr("true")},
				Properties: &armcompute.VirtualMachineProperties{
					HardwareProfile: &armcompute.HardwareProfile{
						VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(opts.InstanceType)),
					},
					OSProfile: &armcompute.OSProfile{
						ComputerName:  to.Ptr(vmName),
						AdminUsername: to.Ptr("loka"),
						CustomData:    to.Ptr(userdata),
					},
					StorageProfile: &armcompute.StorageProfile{
						ImageReference: imageRef,
						OSDisk: &armcompute.OSDisk{
							CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
							ManagedDisk: &armcompute.ManagedDiskParameters{
								StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
							},
						},
					},
				},
			}, nil)
		if err != nil {
			return workers, fmt.Errorf("create VM %s: %w", vmName, err)
		}

		resp, err := poller.PollUntilDone(ctx, nil)
		if err != nil {
			workers = append(workers, &provider.WorkerInfo{
				ID: vmName, Provider: "azure", Region: p.cfg.Location,
				Status: provider.WorkerInfraProvisioning,
			})
			continue
		}

		workers = append(workers, vmToWorkerInfo(&resp.VirtualMachine, p.cfg.Location))
	}

	p.logger.Info("Azure workers provisioned", "count", len(workers))
	return workers, nil
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	p.logger.Info("deprovisioning Azure worker", "vm", workerID)
	poller, err := p.vms.BeginDelete(ctx, p.cfg.ResourceGroup, workerID, nil)
	if err != nil {
		return fmt.Errorf("delete VM: %w", err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	pager := p.vms.NewListPager(p.cfg.ResourceGroup, nil)
	var workers []*provider.WorkerInfo

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list VMs: %w", err)
		}
		for _, vm := range page.Value {
			if vm.Tags != nil {
				if v, ok := vm.Tags["loka-managed"]; ok && *v == "true" {
					workers = append(workers, vmToWorkerInfo(vm, p.cfg.Location))
				}
			}
		}
	}
	return workers, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	vm, err := p.vms.Get(ctx, p.cfg.ResourceGroup, workerID, nil)
	if err != nil {
		return provider.WorkerInfraError, err
	}
	return mapAzureStatus(vm.Properties), nil
}

func vmToWorkerInfo(vm *armcompute.VirtualMachine, location string) *provider.WorkerInfo {
	w := &provider.WorkerInfo{
		ID:       deref(vm.Name, ""),
		Provider: "azure",
		Region:   location,
		Status:   mapAzureStatus(vm.Properties),
		Metadata: map[string]string{},
	}
	if vm.Properties != nil && vm.Properties.HardwareProfile != nil {
		w.Metadata["vm_size"] = string(deref(vm.Properties.HardwareProfile.VMSize, ""))
	}
	return w
}

func mapAzureStatus(props *armcompute.VirtualMachineProperties) provider.WorkerInfraStatus {
	if props == nil {
		return provider.WorkerInfraError
	}
	state := deref(props.ProvisioningState, "")
	switch state {
	case "Creating", "Updating":
		return provider.WorkerInfraProvisioning
	case "Succeeded":
		return provider.WorkerInfraRunning
	case "Deleting":
		return provider.WorkerInfraTerminating
	case "Failed":
		return provider.WorkerInfraError
	default:
		return provider.WorkerInfraRunning
	}
}

func parseImageReference(ref string) *armcompute.ImageReference {
	if ref == "" {
		return &armcompute.ImageReference{
			Publisher: to.Ptr("Canonical"),
			Offer:     to.Ptr("0001-com-ubuntu-server-jammy"),
			SKU:       to.Ptr("22_04-lts"),
			Version:   to.Ptr("latest"),
		}
	}
	parts := strings.SplitN(ref, ":", 4)
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	return &armcompute.ImageReference{
		Publisher: to.Ptr(parts[0]),
		Offer:     to.Ptr(parts[1]),
		SKU:       to.Ptr(parts[2]),
		Version:   to.Ptr(parts[3]),
	}
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

var _ provider.Provider = (*Provider)(nil)
