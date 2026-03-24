package ha

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

func init() {
	RegisterFactory("raft", func(cfg Config) (Coordinator, error) {
		return NewRaftCoordinator(RaftConfig{
			NodeID:    cfg.NodeID,
			BindAddr:  cfg.Address,
			DataDir:   cfg.DataDir,
			Bootstrap: cfg.Bootstrap,
			Peers:     cfg.Peers,
			TLSConfig: cfg.TLSConfig,
		}, slog.Default())
	})
}

// RaftConfig configures the Raft coordinator.
type RaftConfig struct {
	NodeID    string      // Unique node ID (required)
	BindAddr  string      // Raft transport address (e.g. "0.0.0.0:6842")
	DataDir   string      // Directory for Raft log and snapshots
	Bootstrap bool        // Bootstrap as single-node cluster (first node)
	Peers     []string    // Initial peer addresses for joining
	TLSConfig *tls.Config // Optional TLS configuration for Raft transport
}

// tlsStreamLayer implements raft.StreamLayer with TLS encryption.
type tlsStreamLayer struct {
	listener net.Listener
	tlsCfg   *tls.Config
}

func (t *tlsStreamLayer) Accept() (net.Conn, error) {
	conn, err := t.listener.Accept()
	if err != nil {
		return nil, err
	}
	return tls.Server(conn, t.tlsCfg), nil
}

func (t *tlsStreamLayer) Close() error {
	return t.listener.Close()
}

func (t *tlsStreamLayer) Addr() net.Addr {
	return t.listener.Addr()
}

func (t *tlsStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", string(address), timeout)
	if err != nil {
		return nil, err
	}
	return tls.Client(conn, t.tlsCfg), nil
}

// RaftCoordinator implements ha.Coordinator using Hashicorp Raft.
type RaftCoordinator struct {
	raft   *raft.Raft
	fsm    *lokaFSM
	logger *slog.Logger

	mu          sync.RWMutex
	subscribers map[string][]chan []byte
}

// NewRaftCoordinator creates a new Raft-based coordinator.
func NewRaftCoordinator(cfg RaftConfig, logger *slog.Logger) (*RaftCoordinator, error) {
	if cfg.NodeID == "" {
		hostname, _ := os.Hostname()
		cfg.NodeID = hostname
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = "0.0.0.0:6842"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/loka/raft"
	}

	os.MkdirAll(cfg.DataDir, 0o700)

	// Raft config.
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.LogOutput = io.Discard // Use slog instead.

	// BoltDB store for logs + stable store.
	boltStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("raft bolt store: %w", err)
	}

	// Snapshot store.
	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 2, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("raft snapshot store: %w", err)
	}

	// Transport (TLS or plaintext TCP).
	var transport raft.Transport
	if cfg.TLSConfig != nil {
		ln, err := tls.Listen("tcp", cfg.BindAddr, cfg.TLSConfig)
		if err != nil {
			return nil, fmt.Errorf("tls listen: %w", err)
		}
		stream := &tlsStreamLayer{
			listener: ln,
			tlsCfg:   cfg.TLSConfig,
		}
		transport = raft.NewNetworkTransport(stream, 3, 10*time.Second, io.Discard)
	} else {
		addr, err := net.ResolveTCPAddr("tcp", cfg.BindAddr)
		if err != nil {
			return nil, fmt.Errorf("resolve raft addr: %w", err)
		}
		transport, err = raft.NewTCPTransport(cfg.BindAddr, addr, 3, 10*time.Second, io.Discard)
		if err != nil {
			return nil, fmt.Errorf("raft transport: %w", err)
		}
	}

	// FSM.
	fsm := &lokaFSM{
		locks:    make(map[string]lockEntry),
		handlers: make(map[string]func([]byte) interface{}),
	}

	// Create Raft instance.
	r, err := raft.NewRaft(raftCfg, fsm, boltStore, boltStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("create raft: %w", err)
	}

	// Bootstrap if this is the first node.
	if cfg.Bootstrap {
		servers := []raft.Server{{
			ID:      raft.ServerID(cfg.NodeID),
			Address: raft.ServerAddress(cfg.BindAddr),
		}}
		for _, peer := range cfg.Peers {
			servers = append(servers, raft.Server{
				ID:      raft.ServerID(peer),
				Address: raft.ServerAddress(peer),
			})
		}
		r.BootstrapCluster(raft.Configuration{Servers: servers})
	}

	coord := &RaftCoordinator{
		raft:        r,
		fsm:         fsm,
		logger:      logger,
		subscribers: make(map[string][]chan []byte),
	}

	logger.Info("raft coordinator started", "node_id", cfg.NodeID, "bind", cfg.BindAddr, "bootstrap", cfg.Bootstrap)
	return coord, nil
}

// Lock acquires a distributed lock via Raft consensus.
func (c *RaftCoordinator) Lock(ctx context.Context, key string, ttl time.Duration) (func(), error) {
	if c.raft.State() != raft.Leader {
		return nil, fmt.Errorf("not the leader")
	}

	cmd, _ := json.Marshal(fsmCommand{Op: "lock", Key: key, TTL: ttl})
	f := c.raft.Apply(cmd, 5*time.Second)
	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("raft apply lock: %w", err)
	}

	resp := f.Response()
	if resp != nil {
		if errStr, ok := resp.(string); ok && errStr != "" {
			return nil, fmt.Errorf("%s", errStr)
		}
	}

	unlock := func() {
		cmd, _ := json.Marshal(fsmCommand{Op: "unlock", Key: key})
		c.raft.Apply(cmd, 5*time.Second)
	}
	return unlock, nil
}

// Publish broadcasts an event to all subscribers on this node.
// In Raft mode, events are applied through the log so all nodes see them.
func (c *RaftCoordinator) Publish(ctx context.Context, topic string, payload []byte) error {
	cmd, _ := json.Marshal(fsmCommand{Op: "publish", Key: topic, Value: payload})
	f := c.raft.Apply(cmd, 5*time.Second)
	return f.Error()
}

// Subscribe returns a channel for topic events.
func (c *RaftCoordinator) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	ch := make(chan []byte, 64)
	c.mu.Lock()
	c.subscribers[topic] = append(c.subscribers[topic], ch)
	c.mu.Unlock()

	go func() {
		<-ctx.Done()
		c.mu.Lock()
		subs := c.subscribers[topic]
		for i, s := range subs {
			if s == ch {
				c.subscribers[topic] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		close(ch)
	}()

	return ch, nil
}

// ElectLeader uses Raft's built-in leader election.
func (c *RaftCoordinator) ElectLeader(ctx context.Context, name string, leaderFunc func(ctx context.Context)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Wait until we become leader.
		if c.raft.State() == raft.Leader {
			c.logger.Info("this node is the raft leader", "name", name)
			leaderCtx, cancel := context.WithCancel(ctx)

			// Watch for leadership loss.
			go func() {
				leaderCh := c.raft.LeaderCh()
				for {
					select {
					case <-leaderCtx.Done():
						return
					case isLeader := <-leaderCh:
						if !isLeader {
							c.logger.Info("lost raft leadership", "name", name)
							cancel()
							return
						}
					}
				}
			}()

			leaderFunc(leaderCtx)
			cancel()
		}

		// Not leader — wait and check again.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(1 * time.Second):
		}
	}
}

// IsLeader returns true if this node is the Raft leader.
func (c *RaftCoordinator) IsLeader(name string) bool {
	return c.raft.State() == raft.Leader
}

// LeaderAddr returns the Raft leader's address.
func (c *RaftCoordinator) LeaderAddr() string {
	addr, _ := c.raft.LeaderWithID()
	return string(addr)
}

// Apply sends an arbitrary command through Raft consensus.
// The command is applied on all nodes via the FSM.
func (c *RaftCoordinator) Apply(ctx context.Context, cmd []byte) (interface{}, error) {
	if c.raft.State() != raft.Leader {
		return nil, fmt.Errorf("not the leader")
	}
	f := c.raft.Apply(cmd, 10*time.Second)
	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("raft apply: %w", err)
	}
	resp := f.Response()
	if errStr, ok := resp.(string); ok && errStr != "" {
		return nil, fmt.Errorf("%s", errStr)
	}
	return resp, nil
}

// RegisterHandler registers a callback for a given operation type.
// The handler is called on ALL nodes when the FSM applies a command with that op.
func (c *RaftCoordinator) RegisterHandler(op string, fn func(data []byte) interface{}) {
	c.fsm.handlersMu.Lock()
	c.fsm.handlers[op] = fn
	c.fsm.handlersMu.Unlock()
}

// Close shuts down the Raft node.
func (c *RaftCoordinator) Close() error {
	f := c.raft.Shutdown()
	return f.Error()
}

// notify delivers a published message to local subscribers.
func (c *RaftCoordinator) notify(topic string, payload []byte) {
	c.mu.RLock()
	subs := c.subscribers[topic]
	c.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- payload:
		default:
		}
	}
}

// ── FSM ─────────────────────────────────────────────────

type fsmCommand struct {
	Op    string        `json:"op"`    // lock, unlock, publish
	Key   string        `json:"key"`
	Value []byte        `json:"value,omitempty"`
	TTL   time.Duration `json:"ttl,omitempty"`
}

type lockEntry struct {
	ExpiresAt time.Time
}

// lokaFSM is the finite state machine for Raft.
type lokaFSM struct {
	mu    sync.RWMutex
	locks map[string]lockEntry

	// Set by the coordinator after construction.
	coordinator *RaftCoordinator

	// External handlers registered for custom operations.
	handlersMu sync.RWMutex
	handlers   map[string]func([]byte) interface{}
}

func (f *lokaFSM) Apply(l *raft.Log) interface{} {
	var cmd fsmCommand
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		return err.Error()
	}

	switch cmd.Op {
	case "lock":
		f.mu.Lock()
		defer f.mu.Unlock()
		if entry, ok := f.locks[cmd.Key]; ok {
			if time.Now().Before(entry.ExpiresAt) {
				return "lock already held"
			}
		}
		f.locks[cmd.Key] = lockEntry{ExpiresAt: time.Now().Add(cmd.TTL)}
		return ""

	case "unlock":
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.locks, cmd.Key)
		return ""

	case "publish":
		if f.coordinator != nil {
			f.coordinator.notify(cmd.Key, cmd.Value)
		}
		return ""

	default:
		// Dispatch to registered external handlers.
		f.handlersMu.RLock()
		fn, ok := f.handlers[cmd.Op]
		f.handlersMu.RUnlock()
		if ok {
			return fn(l.Data)
		}
		return "unknown op: " + cmd.Op
	}
}

func (f *lokaFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	data, _ := json.Marshal(f.locks)
	return &fsmSnapshot{data: data}, nil
}

func (f *lokaFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return json.Unmarshal(data, &f.locks)
}

type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	_, err := sink.Write(s.data)
	if err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

var _ Coordinator = (*RaftCoordinator)(nil)
