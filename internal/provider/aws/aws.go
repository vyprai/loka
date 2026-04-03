package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/vyprai/loka/internal/provider"
)

// Config holds AWS provider configuration.
type Config struct {
	Region             string
	AccessKey          string
	SecretKey          string
	AMI                string // AMI for worker instances (Ubuntu 22.04 with KVM).
	SecurityGroupID    string
	SubnetID           string
	KeyName            string
	IAMInstanceProfile string
}

// Provider provisions workers on AWS EC2.
type Provider struct {
	cfg    Config
	client *ec2.Client
	logger *slog.Logger
}

// New creates a new AWS provider.
func New(cfg Config, logger *slog.Logger) (*Provider, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return &Provider{
		cfg:    cfg,
		client: ec2.NewFromConfig(awsCfg),
		logger: logger,
	}, nil
}

func (p *Provider) Name() string { return "aws" }

func (p *Provider) Provision(ctx context.Context, opts provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	if opts.InstanceType == "" {
		opts.InstanceType = "i3.metal"
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

	userdata := provider.GenerateCloudInit(provider.BootstrapConfig{
		ControlPlaneAddr: opts.UserData, // CP address passed via UserData field.
		Token:            opts.Labels["_token"],
		Provider:         "aws",
		Labels:           opts.Labels,
	})

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(p.cfg.AMI),
		InstanceType: types.InstanceType(opts.InstanceType),
		MinCount:     aws.Int32(int32(opts.Count)),
		MaxCount:     aws.Int32(int32(opts.Count)),
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(userdata))),
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeInstance,
			Tags: []types.Tag{
				{Key: aws.String("loka-managed"), Value: aws.String("true")},
				{Key: aws.String("Name"), Value: aws.String("loka-worker")},
			},
		}},
	}

	if p.cfg.SubnetID != "" {
		input.SubnetId = aws.String(p.cfg.SubnetID)
	}
	if p.cfg.SecurityGroupID != "" {
		input.SecurityGroupIds = []string{p.cfg.SecurityGroupID}
	}
	if p.cfg.KeyName != "" || opts.SSHKeyName != "" {
		keyName := p.cfg.KeyName
		if opts.SSHKeyName != "" {
			keyName = opts.SSHKeyName
		}
		input.KeyName = aws.String(keyName)
	}
	if p.cfg.IAMInstanceProfile != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Name: aws.String(p.cfg.IAMInstanceProfile),
		}
	}

	result, err := p.client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("ec2.RunInstances: %w", err)
	}

	var workers []*provider.WorkerInfo
	for _, inst := range result.Instances {
		w := &provider.WorkerInfo{
			ID:       aws.ToString(inst.InstanceId),
			Provider: "aws",
			Region:   region,
			Status:   mapEC2State(inst.State),
			Metadata: map[string]string{
				"instance_type": string(inst.InstanceType),
				"ami":           aws.ToString(inst.ImageId),
			},
		}
		if inst.PublicIpAddress != nil {
			w.ExternalIP = *inst.PublicIpAddress
		}
		if inst.PrivateIpAddress != nil {
			w.InternalIP = *inst.PrivateIpAddress
		}
		if inst.Placement != nil {
			w.Zone = aws.ToString(inst.Placement.AvailabilityZone)
		}
		workers = append(workers, w)
	}

	p.logger.Info("AWS workers provisioned", "count", len(workers))
	return workers, nil
}

func (p *Provider) Deprovision(ctx context.Context, workerID string) error {
	p.logger.Info("deprovisioning AWS worker", "worker", workerID)
	_, err := p.client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{workerID},
	})
	if err != nil {
		return fmt.Errorf("ec2.TerminateInstances: %w", err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context) ([]*provider.WorkerInfo, error) {
	result, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:loka-managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2.DescribeInstances: %w", err)
	}

	var workers []*provider.WorkerInfo
	for _, res := range result.Reservations {
		for _, inst := range res.Instances {
			w := &provider.WorkerInfo{
				ID:       aws.ToString(inst.InstanceId),
				Provider: "aws",
				Status:   mapEC2State(inst.State),
				Metadata: map[string]string{"instance_type": string(inst.InstanceType)},
			}
			if inst.PublicIpAddress != nil {
				w.ExternalIP = *inst.PublicIpAddress
			}
			if inst.PrivateIpAddress != nil {
				w.InternalIP = *inst.PrivateIpAddress
			}
			if inst.Placement != nil {
				w.Zone = aws.ToString(inst.Placement.AvailabilityZone)
			}
			workers = append(workers, w)
		}
	}
	return workers, nil
}

func (p *Provider) WorkerStatus(ctx context.Context, workerID string) (provider.WorkerInfraStatus, error) {
	result, err := p.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{workerID},
	})
	if err != nil {
		return provider.WorkerInfraError, fmt.Errorf("ec2.DescribeInstances: %w", err)
	}
	for _, res := range result.Reservations {
		for _, inst := range res.Instances {
			return mapEC2State(inst.State), nil
		}
	}
	return provider.WorkerInfraTerminated, nil
}

func mapEC2State(state *types.InstanceState) provider.WorkerInfraStatus {
	if state == nil {
		return provider.WorkerInfraError
	}
	switch state.Name {
	case types.InstanceStateNamePending:
		return provider.WorkerInfraProvisioning
	case types.InstanceStateNameRunning:
		return provider.WorkerInfraRunning
	case types.InstanceStateNameShuttingDown, types.InstanceStateNameStopping:
		return provider.WorkerInfraTerminating
	case types.InstanceStateNameTerminated, types.InstanceStateNameStopped:
		return provider.WorkerInfraTerminated
	default:
		return provider.WorkerInfraError
	}
}

var _ provider.Provider = (*Provider)(nil)
