package local

import (
	"context"

	"github.com/vyprai/loka/internal/provider"
)

// Provider is a no-op provider for local development.
// Workers are registered manually or via the embedded local worker.
type Provider struct{}

func New() *Provider { return &Provider{} }

func (p *Provider) Name() string { return "local" }

func (p *Provider) Provision(_ context.Context, _ provider.ProvisionOpts) ([]*provider.WorkerInfo, error) {
	return nil, provider.ErrNotSupported
}

func (p *Provider) Deprovision(_ context.Context, _ string) error {
	return provider.ErrNotSupported
}

func (p *Provider) List(_ context.Context) ([]*provider.WorkerInfo, error) {
	return nil, nil
}

func (p *Provider) WorkerStatus(_ context.Context, _ string) (provider.WorkerInfraStatus, error) {
	return provider.WorkerInfraRunning, nil
}

var _ provider.Provider = (*Provider)(nil)
