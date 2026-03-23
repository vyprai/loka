package ha

import (
	"context"
	"sync"
	"time"
)

func init() {
	RegisterFactory("local", func(_ Config) (Coordinator, error) {
		return NewLocalCoordinator(), nil
	})
}

// LocalCoordinator is an in-process coordinator for single-node dev mode.
// It uses sync primitives instead of distributed consensus.
type LocalCoordinator struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	leaders  map[string]bool
	subs     map[string][]chan []byte
	subsMu   sync.RWMutex
}

// NewLocalCoordinator creates a new in-process coordinator.
func NewLocalCoordinator() *LocalCoordinator {
	return &LocalCoordinator{
		locks:   make(map[string]*sync.Mutex),
		leaders: make(map[string]bool),
		subs:    make(map[string][]chan []byte),
	}
}

func (c *LocalCoordinator) Lock(_ context.Context, key string, _ time.Duration) (func(), error) {
	c.mu.Lock()
	m, ok := c.locks[key]
	if !ok {
		m = &sync.Mutex{}
		c.locks[key] = m
	}
	c.mu.Unlock()

	m.Lock()
	return func() { m.Unlock() }, nil
}

func (c *LocalCoordinator) Publish(_ context.Context, topic string, payload []byte) error {
	c.subsMu.RLock()
	defer c.subsMu.RUnlock()

	for _, ch := range c.subs[topic] {
		select {
		case ch <- payload:
		default:
			// Drop if subscriber is slow.
		}
	}
	return nil
}

func (c *LocalCoordinator) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	ch := make(chan []byte, 64)

	c.subsMu.Lock()
	c.subs[topic] = append(c.subs[topic], ch)
	c.subsMu.Unlock()

	go func() {
		<-ctx.Done()
		c.subsMu.Lock()
		defer c.subsMu.Unlock()
		subs := c.subs[topic]
		for i, s := range subs {
			if s == ch {
				c.subs[topic] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		close(ch)
	}()

	return ch, nil
}

func (c *LocalCoordinator) ElectLeader(ctx context.Context, name string, leaderFunc func(ctx context.Context)) error {
	// In single-node mode, we are always the leader.
	c.mu.Lock()
	c.leaders[name] = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.leaders[name] = false
		c.mu.Unlock()
	}()

	leaderFunc(ctx)
	return nil
}

func (c *LocalCoordinator) IsLeader(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaders[name]
}

func (c *LocalCoordinator) Close() error {
	return nil
}

// Compile-time check.
var _ Coordinator = (*LocalCoordinator)(nil)
