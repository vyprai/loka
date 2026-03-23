package aws

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/provider"
)

// Config holds AWS provider configuration.
type Config struct {
	Region          string
	AMI             string // AMI for worker instances (Amazon Linux 2 with KVM).
	SecurityGroupID string
	SubnetID        string
	KeyName         string
	IAMInstanceProfile string
}

// Provider provisions workers on AWS EC2.
// Uses bare-metal instances (.metal) for KVM support.
type Provider struct {
	cfg    Config
	logger *slog.Logger
	// In production: aws SDK client would go here.
	// client *ec2.Client
}

// New creates a new AWS provider.
func New(cfg Config, logger *slog.Logger) *Provider {
	return &Provider{cfg: cfg, logger: logger}
}

func (p *Provider) Name() string { return "aws" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "i3.metal" // Default bare-metal for KVM.
	}
	region := opts.Region
	if region == "" {
		region = p.cfg.Region
	}
	if opts.Count == 0 {
		opts.Count = 1
	}

	p.logger.Info("provisioning AWS workers",
		"count", opts.Count,
		"instance_type", opts.InstanceType,
		"region", region,
	)

	// Generate bootstrap userdata.
	userdata := provider.GenerateCloudInit(provider.BootstrapConfig{
		ControlPlaneAddr: "cp.loka.internal:6841", // Would come from config.
		Token:            opts.UserData,            // Registration token passed via UserData.
		Provider:         "aws",
		Labels:           opts.Labels,
	})

	// TODO: Replace with real AWS SDK calls:
	// ec2.RunInstances({
	//   ImageId:          p.cfg.AMI,
	//   InstanceType:     opts.InstanceType,
	//   MinCount:         opts.Count,
	//   MaxCount:         opts.Count,
	//   SubnetId:         p.cfg.SubnetID,
	//   SecurityGroupIds: []string{p.cfg.SecurityGroupID},
	//   KeyName:          p.cfg.KeyName,
	//   UserData:         base64.StdEncoding.EncodeToString([]byte(userdata)),
	//   IamInstanceProfile: {Name: p.cfg.IAMInstanceProfile},
	// })

	_ = userdata

	var workers []*provider.WorkerInfo
	for i := 0; i < opts.Count; i++ {
		workers = append(workers, &provider.WorkerInfo{
			ID:       "aws-" + uuid.New().String()[:8],
			Provider: "aws",
			Region:   region,
			Status:   provider.WorkerInfraProvisioning,
			Metadata: map[string]string{
				"instance_type": opts.InstanceType,
				"ami":           p.cfg.AMI,
			},
		})
	}

	return workers, fmt.Errorf("AWS provisioning not yet implemented (requires AWS credentials)")
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	p.logger.Info("deprovisioning AWS worker", "worker", workerID)
	// TODO: ec2.TerminateInstances({InstanceIds: []string{workerID}})
	return fmt.Errorf("AWS deprovisioning not yet implemented")
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	// TODO: ec2.DescribeInstances with loka tags
	return nil, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	// TODO: ec2.DescribeInstanceStatus
	return provider.WorkerInfraRunning, nil
}

var _ provider.Provider = (*Provider)(nil)
