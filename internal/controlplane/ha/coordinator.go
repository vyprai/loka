package ha

import (
	"context"
	"time"
)

// Coordinator provides distributed coordination primitives.
// Implementations: LocalCoordinator (single-node dev), RaftCoordinator (production HA).
type Coordinator interface {
	// Lock acquires a distributed lock. Returns unlock function.
	// The lock is held until unlock() is called or the TTL expires.
	Lock(ctx context.Context, key string, ttl time.Duration) (unlock func(), err error)

	// Publish sends an event to a topic.
	Publish(ctx context.Context, topic string, payload []byte) error

	// Subscribe returns a channel that receives events for a topic.
	// The channel is closed when the context is canceled.
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)

	// ElectLeader participates in leader election.
	// leaderFunc runs only while this instance holds leadership.
	// Blocks until the context is canceled.
	ElectLeader(ctx context.Context, name string, leaderFunc func(ctx context.Context)) error

	// IsLeader returns whether this instance currently holds leadership.
	IsLeader(name string) bool

	Close() error
}

// Cache provides optional caching for hot data.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}
