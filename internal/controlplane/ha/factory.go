package ha

import (
	"crypto/tls"
	"fmt"
)

// Config holds coordinator configuration.
type Config struct {
	Type      string   // "local" or "raft"
	Address   string   // Raft bind address (e.g. "0.0.0.0:6842")
	NodeID    string   // Unique node ID for Raft
	DataDir   string   // Raft data directory
	Bootstrap bool     // Bootstrap as first node
	Peers     []string    // Initial peer addresses
	TLSConfig *tls.Config // Optional TLS configuration for Raft transport
}

// Factory function type.
type FactoryFunc func(cfg Config) (Coordinator, error)

var factories = map[string]FactoryFunc{}

// RegisterFactory registers a coordinator factory.
func RegisterFactory(typeName string, fn FactoryFunc) {
	factories[typeName] = fn
}

// Open creates a coordinator based on config.
func Open(cfg Config) (Coordinator, error) {
	fn, ok := factories[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("unknown coordinator type: %s (available: local, raft)", cfg.Type)
	}
	return fn(cfg)
}
