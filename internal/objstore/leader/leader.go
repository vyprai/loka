package leader

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/vyprai/loka/internal/objstore"
	proxyobjstore "github.com/vyprai/loka/internal/objstore/proxy"
)

// LeaderCheck provides leader status and address.
type LeaderCheck interface {
	IsLeader(name string) bool
	LeaderAddr() string
}

// Store wraps a local object store and a proxy to the leader.
// Reads always go to the local store first (for cache hits).
// Writes are forwarded to the leader in HA mode when this node is not the leader.
// When this node IS the leader (or in single mode), writes go directly to local.
type Store struct {
	local   objstore.ObjectStore
	proxy   *proxyobjstore.Store
	leader  LeaderCheck
	name    string // Leadership election name (e.g. "control-plane").
	scheme  string // "https" or "http" for constructing leader URL.
	apiPort string // API port to construct leader URL.
}

// Config configures the leader-aware object store.
type Config struct {
	Local   objstore.ObjectStore // The real backend store.
	Leader  LeaderCheck          // HA coordinator for leader checks.
	Name    string               // Leadership election name.
	Scheme  string               // URL scheme for leader proxy ("https" or "http").
	APIPort string               // API listen port (e.g. "6840").
	Token   string               // Internal auth token for proxy requests.
}

// New creates a leader-aware object store.
// In single mode (leader is nil or always returns true), this is a passthrough.
func New(cfg Config) *Store {
	s := &Store{
		local:   cfg.Local,
		leader:  cfg.Leader,
		name:    cfg.Name,
		scheme:  cfg.Scheme,
		apiPort: cfg.APIPort,
	}
	if cfg.Leader != nil {
		s.proxy = proxyobjstore.New(proxyobjstore.Config{
			Token: cfg.Token,
		})
	}
	return s
}

func (s *Store) isLeader() bool {
	if s.leader == nil {
		return true
	}
	return s.leader.IsLeader(s.name)
}

// writeStore returns the store to use for write operations.
// Leader writes locally; non-leaders forward to the leader.
func (s *Store) writeStore() objstore.ObjectStore {
	if s.isLeader() {
		return s.local
	}
	// Update proxy URL to point to current leader.
	addr := s.leader.LeaderAddr()
	if addr != "" && s.proxy != nil {
		s.proxy.SetBaseURL(s.scheme + "://" + addr)
		// Leader addr from Raft is the Raft port, but we need the API port.
		// Use the host from leader addr with the configured API port.
		host, _, _ := splitHostPort(addr)
		if host != "" {
			s.proxy.SetBaseURL(s.scheme + "://" + host + ":" + s.apiPort)
		}
		return s.proxy
	}
	// Fallback to local if leader address unknown.
	return s.local
}

func (s *Store) Put(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	return s.writeStore().Put(ctx, bucket, key, reader, size)
}

func (s *Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	// Try local first (cache), then fall back to leader proxy.
	rc, err := s.local.Get(ctx, bucket, key)
	if err == nil {
		return rc, nil
	}
	if !s.isLeader() && s.proxy != nil {
		return s.writeStore().Get(ctx, bucket, key)
	}
	return nil, err
}

func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	return s.writeStore().Delete(ctx, bucket, key)
}

func (s *Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	// Check local first.
	exists, err := s.local.Exists(ctx, bucket, key)
	if err == nil && exists {
		return true, nil
	}
	if !s.isLeader() && s.proxy != nil {
		return s.writeStore().Exists(ctx, bucket, key)
	}
	return exists, err
}

func (s *Store) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	return s.writeStore().GetPresignedURL(ctx, bucket, key, expiry)
}

func (s *Store) List(ctx context.Context, bucket, prefix string) ([]objstore.ObjectInfo, error) {
	// Always list from the authoritative store.
	return s.writeStore().List(ctx, bucket, prefix)
}

// splitHostPort extracts host and port from an address string.
// Uses net.SplitHostPort which correctly handles IPv6 addresses like [::1]:8080.
func splitHostPort(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// If no port separator found, return the whole address as host.
		return addr, "", nil
	}
	return host, port, nil
}

var _ objstore.ObjectStore = (*Store)(nil)
