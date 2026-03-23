package selfmanaged

import (
	"context"
	"fmt"

	"github.com/vyprai/loka/internal/provider"
	"github.com/vyprai/loka/internal/store"
)

// Provider handles self-managed workers that connect inbound.
// It validates registration tokens but does not provision infrastructure.
type Provider struct {
	store store.Store
}

// New creates a new self-managed provider.
func New(s store.Store) *Provider {
	return &Provider{store: s}
}

func (p *Provider) Name() string { return "selfmanaged" }

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

// ValidateToken validates a registration token for a self-managed worker.
func (p *Provider) ValidateToken(ctx context.Context, token string) (any, error) {
	t, err := p.store.Tokens().GetByToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("invalid token")
	}
	if !t.IsValid() {
		if t.Used {
			return nil, fmt.Errorf("token already used")
		}
		return nil, fmt.Errorf("token expired")
	}
	return t, nil
}

var _ provider.Provider = (*Provider)(nil)
var _ provider.SelfManagedProvider = (*Provider)(nil)
