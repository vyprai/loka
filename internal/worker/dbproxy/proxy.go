// Package dbproxy implements a database-aware TCP proxy that runs on each
// worker. It provides connection pooling and read/write split routing for
// postgres, mysql, and redis databases.
//
// The proxy listens on localhost ports for each database that services on
// this worker need to access. It routes writes to the primary and reads
// round-robin to replicas, using protocol-level query inspection.
package dbproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Backend represents a database server (primary or replica).
type Backend struct {
	ID       string // Service ID.
	Name     string // Service name.
	Address  string // host:port to connect to.
	Role     string // "primary" or "replica"
	Engine   string // "postgres", "mysql", "redis"
	Healthy  bool
	LastPing time.Time
}

// Route defines a database proxy route with primary + replicas.
type Route struct {
	Name       string // Database name (used for listener port lookup).
	Engine     string // "postgres", "mysql", "redis"
	ListenPort int    // Local port to listen on.
	Primary    *Backend
	Replicas   []*Backend
	mu         sync.RWMutex
	rrCounter  uint64
}

// PickBackend selects a backend based on query type.
// Writes always go to primary. Reads round-robin across replicas (or primary if no replicas).
func (r *Route) PickBackend(isRead bool) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !isRead || len(r.Replicas) == 0 {
		return r.Primary
	}

	// Filter to healthy replicas.
	var healthy []*Backend
	for _, rep := range r.Replicas {
		if rep.Healthy {
			healthy = append(healthy, rep)
		}
	}
	if len(healthy) == 0 {
		return r.Primary // Graceful degradation: reads go to primary.
	}

	idx := atomic.AddUint64(&r.rrCounter, 1) % uint64(len(healthy))
	return healthy[idx]
}

// Proxy is the worker-side database proxy.
type Proxy struct {
	routes    map[string]*Route // name → route
	listeners map[string]net.Listener
	mu        sync.RWMutex
	logger    *slog.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// New creates a new database proxy.
func New(logger *slog.Logger) *Proxy {
	ctx, cancel := context.WithCancel(context.Background())
	return &Proxy{
		routes:    make(map[string]*Route),
		listeners: make(map[string]net.Listener),
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// AddRoute adds or updates a database route. Starts a listener if new.
func (p *Proxy) AddRoute(route *Route) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing, ok := p.routes[route.Name]
	if ok {
		// Update backends on existing route.
		existing.mu.Lock()
		existing.Primary = route.Primary
		existing.Replicas = route.Replicas
		existing.mu.Unlock()
		return nil
	}

	// New route — start listener.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", route.ListenPort))
	if err != nil {
		return fmt.Errorf("listen for %s on port %d: %w", route.Name, route.ListenPort, err)
	}

	p.routes[route.Name] = route
	p.listeners[route.Name] = listener

	p.wg.Add(1)
	go p.acceptLoop(route, listener)

	p.logger.Info("db proxy: route added",
		"database", route.Name, "engine", route.Engine, "port", route.ListenPort,
		"primary", route.Primary.Address, "replicas", len(route.Replicas))
	return nil
}

// RemoveRoute stops and removes a database route.
func (p *Proxy) RemoveRoute(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if listener, ok := p.listeners[name]; ok {
		listener.Close()
		delete(p.listeners, name)
	}
	delete(p.routes, name)
}

// Close shuts down all listeners and waits for connections to drain.
func (p *Proxy) Close() {
	p.cancel()
	p.mu.Lock()
	for _, l := range p.listeners {
		l.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *Proxy) acceptLoop(route *Route, listener net.Listener) {
	defer p.wg.Done()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-p.ctx.Done():
				return
			default:
				p.logger.Debug("db proxy: accept error", "database", route.Name, "error", err)
				return
			}
		}
		go p.handleConnection(route, conn)
	}
}

func (p *Proxy) handleConnection(route *Route, clientConn net.Conn) {
	defer clientConn.Close()

	// Dispatch to engine-specific protocol handler for read/write split.
	switch route.Engine {
	case "postgres":
		newPostgresProxy(route).HandleConnection(clientConn)
	case "mysql":
		newMySQLProxy(route).HandleConnection(clientConn)
	case "redis":
		newRedisProxy(route).HandleConnection(clientConn)
	default:
		// Fallback: simple TCP tunnel to primary (no read/write split).
		p.simpleTCPProxy(route, clientConn)
	}
}

// simpleTCPProxy is a dumb bidirectional TCP tunnel to the primary backend.
func (p *Proxy) simpleTCPProxy(route *Route, clientConn net.Conn) {
	backend := route.PickBackend(false)
	if backend == nil {
		p.logger.Error("db proxy: no backend available", "database", route.Name)
		return
	}

	backendConn, err := net.DialTimeout("tcp", backend.Address, 5*time.Second)
	if err != nil {
		p.logger.Error("db proxy: backend connect failed",
			"database", route.Name, "backend", backend.Address, "error", err)
		return
	}
	defer backendConn.Close()

	done := make(chan struct{})
	go func() {
		io.Copy(backendConn, clientConn)
		close(done)
	}()
	io.Copy(clientConn, backendConn)
	<-done
}

// StartHealthCheck begins periodic health checking of all backends.
func (p *Proxy) StartHealthCheck() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-p.ctx.Done():
				return
			case <-ticker.C:
				p.checkHealth()
			}
		}
	}()
}

func (p *Proxy) checkHealth() {
	p.mu.RLock()
	routes := make([]*Route, 0, len(p.routes))
	for _, r := range p.routes {
		routes = append(routes, r)
	}
	p.mu.RUnlock()

	for _, route := range routes {
		route.mu.Lock()
		// Check primary.
		if route.Primary != nil {
			route.Primary.Healthy = p.pingBackend(route.Primary)
			route.Primary.LastPing = time.Now()
		}
		// Check replicas.
		for _, rep := range route.Replicas {
			rep.Healthy = p.pingBackend(rep)
			rep.LastPing = time.Now()
		}
		route.mu.Unlock()
	}
}

func (p *Proxy) pingBackend(backend *Backend) bool {
	conn, err := net.DialTimeout("tcp", backend.Address, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
