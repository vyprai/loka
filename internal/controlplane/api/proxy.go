package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
	"github.com/vyprai/loka/internal/store"
)

// DomainProxy is a reverse proxy that routes HTTP requests by domain
// to specific ports inside session VMs or deployed services.
//
// For service routes, the proxy supports cold-start wake-on-request:
// if a service is idle, the first request triggers a wake and blocks
// until the service is ready (up to 30s).
//
// Example: my-app.loka -> session abc123, port 5000
// CertRegenerator is called when a new domain route is added so the TLS cert
// can be regenerated to include the domain as a SAN.
type CertRegenerator func(domains []string)

type DomainProxy struct {
	sm         *session.Manager
	svcMgr     *service.Manager // Service manager for cold-start wake.
	registry   *worker.Registry
	logger     *slog.Logger
	certRegen  CertRegenerator // Called when routes change to update TLS cert.

	mu     sync.RWMutex
	routes map[string]*loka.DomainRoute // domain -> route

	// wakeMu protects wakeInFlight to coalesce concurrent wake requests.
	wakeMu       sync.Mutex
	wakeInFlight map[string]*wakeState // serviceID -> in-progress wake

	// Round-robin counter for distributing across service instances.
	rrCounter uint64

	// Instance cache: service ID → running instances (primary + replicas).
	instanceCache   sync.Map // string → *instanceCacheEntry
	instanceCacheTTL time.Duration

	// httpClient is a shared, reusable HTTP client for proxying requests.
	// Avoids creating a new client (and transport) per request, enabling
	// connection reuse and reducing GC pressure.
	httpClient *http.Client
}

// wakeState tracks an in-progress wake so concurrent requests for the same
// idle service share a single Wake call.
type wakeState struct {
	done chan struct{} // closed when wake completes
	err  error        // non-nil if wake failed
}

// NewDomainProxy creates a domain-based reverse proxy.
func NewDomainProxy(sm *session.Manager, registry *worker.Registry, logger *slog.Logger, opts ...DomainProxyOpts) *DomainProxy {
	var o DomainProxyOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	return &DomainProxy{
		sm:               sm,
		svcMgr:           o.ServiceManager,
		registry:         registry,
		logger:           logger,
		routes:           make(map[string]*loka.DomainRoute),
		wakeInFlight:     make(map[string]*wakeState),
		instanceCacheTTL: 5 * time.Second,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// StartRouteReaper periodically removes stale routes for sessions/services that no longer exist.
func (p *DomainProxy) StartRouteReaper() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			p.mu.Lock()
			for domain, route := range p.routes {
				if route.Type == loka.DomainRouteSession && route.SessionID != "" {
					if _, err := p.sm.Get(context.Background(), route.SessionID); err != nil {
						p.logger.Info("reaping stale route (session gone)", "domain", domain)
						delete(p.routes, domain)
					}
				}
			}
			p.mu.Unlock()
		}
	}()
}

// DomainProxyOpts holds optional configuration for the domain proxy.
type DomainProxyOpts struct {
	ServiceManager *service.Manager
}

// SetCertRegenerator sets a callback to regenerate TLS certs when routes change.
func (p *DomainProxy) SetCertRegenerator(fn CertRegenerator) {
	p.certRegen = fn
}

// allDomains returns all registered domain names.
func (p *DomainProxy) allDomains() []string {
	var domains []string
	for d := range p.routes {
		domains = append(domains, d)
	}
	return domains
}

// AddRoute registers a domain -> session/service:port mapping.
func (p *DomainProxy) AddRoute(route *loka.DomainRoute) {
	p.mu.Lock()
	p.routes[route.Domain] = route
	domains := p.allDomains()
	p.mu.Unlock()

	// Regenerate TLS cert to include the new domain as a SAN.
	if p.certRegen != nil {
		p.certRegen(domains)
	}

	routeType := string(route.Type)
	if routeType == "" {
		routeType = "session"
	}
	targetID := route.SessionID
	if route.Type == loka.DomainRouteService {
		targetID = route.ServiceID
	}
	p.logger.Info("domain route added",
		"domain", route.Domain,
		"type", routeType,
		"target", targetID,
		"port", route.RemotePort,
		"domain", route.Domain)
}

// RemoveRoute removes a domain mapping.
func (p *DomainProxy) RemoveRoute(domain string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.routes[domain]
	delete(p.routes, domain)
	return ok
}

// GetRoute returns a route by domain.
func (p *DomainProxy) GetRoute(domain string) *loka.DomainRoute {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.routes[domain]
}

// ListRoutes returns all active routes.
func (p *DomainProxy) ListRoutes() []*loka.DomainRoute {
	p.mu.RLock()
	defer p.mu.RUnlock()
	routes := make([]*loka.DomainRoute, 0, len(p.routes))
	for _, r := range p.routes {
		routes = append(routes, r)
	}
	return routes
}

// Handler returns the HTTP handler for the reverse proxy.
// It inspects the Host header to determine which session/port to proxy to.
func (p *DomainProxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Strip port from host.
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		// Match host directly against registered domains.
		route := p.GetRoute(host)
		if route == nil {
			// For localhost/127.0.0.1, show available routes.
			if host == "localhost" || host == "127.0.0.1" {
				routes := p.ListRoutes()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "<html><head><title>LOKA</title></head><body>")
				fmt.Fprintf(w, "<h2>LOKA Domain Proxy</h2>")
				if len(routes) == 0 {
					fmt.Fprintf(w, "<p>No services deployed. Deploy with: <code>loka deploy</code></p>")
				} else {
					fmt.Fprintf(w, "<ul>")
					for _, rt := range routes {
						fmt.Fprintf(w, "<li><a href=\"http://%s\">%s</a> → port %d</li>", rt.Domain, rt.Domain, rt.RemotePort)
					}
					fmt.Fprintf(w, "</ul>")
				}
				fmt.Fprintf(w, "</body></html>")
				return
			}
			http.Error(w, fmt.Sprintf("No route for host %s", host), http.StatusNotFound)
			return
		}

		// Dispatch based on route type.
		if route.Type == loka.DomainRouteService {
			p.handleServiceRoute(w, r, route)
			return
		}

		// Default: session route.
		p.handleSessionRoute(w, r, route)
	})
}

// handleSessionRoute proxies a request to a session VM (original behavior).
func (p *DomainProxy) handleSessionRoute(w http.ResponseWriter, r *http.Request, route *loka.DomainRoute) {
	// Verify session is still running.
	sess, err := p.sm.Get(r.Context(), route.SessionID)
	if err != nil || sess.Status != loka.SessionStatusRunning {
		http.Error(w, "Session is not running", http.StatusServiceUnavailable)
		return
	}

	// Find the worker.
	wc, ok := p.registry.Get(sess.WorkerID)
	if !ok {
		http.Error(w, "Worker not available", http.StatusServiceUnavailable)
		return
	}

	// Proxy the request to the worker VM's port.
	// Use private IP for internal routing if available.
	workerAddr := wc.Worker.PrivateIP
	if workerAddr == "" {
		workerAddr = wc.Worker.IPAddress
	}
	targetAddr := fmt.Sprintf("%s:%d", workerAddr, route.RemotePort)
	p.proxyHTTP(w, r, targetAddr, route, false)
}

// instanceCacheEntry holds cached running instances for round-robin.
type instanceCacheEntry struct {
	services  []*loka.Service
	fetchedAt time.Time
}

// findRunningInstances returns all running instances of a service (primary + replicas).
// Results are cached for instanceCacheTTL to avoid per-request DB queries.
func (p *DomainProxy) findRunningInstances(ctx context.Context, svc *loka.Service) []*loka.Service {
	if p.svcMgr == nil {
		return []*loka.Service{svc}
	}

	// Use the primary ID for cache key (replicas point to it).
	cacheKey := svc.ID
	if svc.ParentServiceID != "" {
		cacheKey = svc.ParentServiceID
	}

	// Check cache.
	if cached, ok := p.instanceCache.Load(cacheKey); ok {
		entry := cached.(*instanceCacheEntry)
		if time.Since(entry.fetchedAt) < p.instanceCacheTTL {
			return entry.services
		}
	}

	// Fetch from store: primary + replicas.
	running := loka.ServiceStatusRunning
	all, _, _ := p.svcMgr.List(ctx, store.ServiceFilter{
		PrimaryID: &cacheKey,
		Status:    &running,
	})

	// Include the primary itself if it's running.
	instances := []*loka.Service{}
	primary, err := p.svcMgr.Get(ctx, cacheKey)
	if err == nil && primary.Status == loka.ServiceStatusRunning {
		instances = append(instances, primary)
	}
	instances = append(instances, all...)

	// Cache the result.
	p.instanceCache.Store(cacheKey, &instanceCacheEntry{
		services:  instances,
		fetchedAt: time.Now(),
	})

	if len(instances) == 0 {
		return []*loka.Service{svc}
	}
	return instances
}

// pickInstance selects a service instance using sticky session cookie or round-robin.
func (p *DomainProxy) pickInstance(r *http.Request, instances []*loka.Service) *loka.Service {
	// Check sticky session cookie.
	if cookie, err := r.Cookie("X-Loka-Instance"); err == nil && cookie.Value != "" {
		for _, inst := range instances {
			if inst.ID == cookie.Value {
				return inst // Sticky: route to same instance.
			}
		}
		// Instance gone — fall through to round-robin.
	}
	// Round-robin.
	idx := atomic.AddUint64(&p.rrCounter, 1) % uint64(len(instances))
	return instances[idx]
}

// handleServiceRoute proxies a request to a deployed service, with cold-start
// wake-on-request support. If the service is idle, it wakes it and waits for
// readiness before proxying.
func (p *DomainProxy) handleServiceRoute(w http.ResponseWriter, r *http.Request, route *loka.DomainRoute) {
	if p.svcMgr == nil {
		http.Error(w, "Service routing not available", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	svc, err := p.svcMgr.Get(ctx, route.ServiceID)
	if err != nil {
		http.Error(w, "Service not found", http.StatusNotFound)
		return
	}

	coldStart := false

	switch svc.Status {
	case loka.ServiceStatusRunning:
		// Ready to proxy.

	case loka.ServiceStatusIdle:
		// Cold-start: wake the service and wait.
		triggered, wakeErr := p.wakeAndWait(ctx, route.ServiceID)
		if wakeErr != nil {
			p.logger.Error("cold-start wake failed",
				"service", route.ServiceID, "error", wakeErr)
			w.Header().Set("Retry-After", "5")
			http.Error(w, fmt.Sprintf("Service wake failed: %v", wakeErr), http.StatusServiceUnavailable)
			return
		}
		coldStart = triggered
		// Re-fetch to get updated worker assignment.
		svc, err = p.svcMgr.Get(ctx, route.ServiceID)
		if err != nil {
			http.Error(w, "Service not found after wake", http.StatusServiceUnavailable)
			return
		}

	case loka.ServiceStatusWaking:
		// Another request already triggered a wake; just wait for it.
		triggered, wakeErr := p.wakeAndWait(ctx, route.ServiceID)
		if wakeErr != nil {
			p.logger.Error("waiting for waking service failed",
				"service", route.ServiceID, "error", wakeErr)
			w.Header().Set("Retry-After", "5")
			http.Error(w, fmt.Sprintf("Service wake failed: %v", wakeErr), http.StatusServiceUnavailable)
			return
		}
		coldStart = triggered
		svc, err = p.svcMgr.Get(ctx, route.ServiceID)
		if err != nil {
			http.Error(w, "Service not found after wake", http.StatusServiceUnavailable)
			return
		}

	default:
		// deploying, stopped, error -- not routable.
		w.Header().Set("Retry-After", "10")
		http.Error(w, fmt.Sprintf("Service is %s", svc.Status), http.StatusServiceUnavailable)
		return
	}

	// Load balance: select an instance (sticky session or round-robin).
	instances := p.findRunningInstances(ctx, svc)
	if len(instances) > 1 {
		svc = p.pickInstance(r, instances)
	}

	// Find the worker.
	if svc.WorkerID == "" {
		http.Error(w, "Service has no assigned worker", http.StatusServiceUnavailable)
		return
	}
	wc, ok := p.registry.Get(svc.WorkerID)
	if !ok {
		http.Error(w, "Worker not available", http.StatusServiceUnavailable)
		return
	}

	// Touch the service idle timer BEFORE proxying so that in-flight requests
	// keep the service active and the idle monitor sees recent activity.
	if err := p.svcMgr.Touch(ctx, route.ServiceID); err != nil {
		p.logger.Warn("failed to touch service", "service", route.ServiceID, "error", err)
	}

	// Route to the service: prefer direct VM guest IP (TAP networking),
	// fall back to vsock-tunnelled forward port, then to worker IP.
	var targetAddr string
	if svc.GuestIP != "" {
		targetAddr = fmt.Sprintf("%s:%d", svc.GuestIP, route.RemotePort)
	} else if svc.ForwardPort > 0 {
		// Use worker's private IP for remote workers, localhost for embedded.
		workerAddr := wc.Worker.PrivateIP
		if workerAddr == "" {
			workerAddr = wc.Worker.IPAddress
		}
		if workerAddr == "" || workerAddr == "127.0.0.1" {
			targetAddr = fmt.Sprintf("127.0.0.1:%d", svc.ForwardPort)
		} else {
			targetAddr = fmt.Sprintf("%s:%d", workerAddr, svc.ForwardPort)
		}
	} else {
		targetAddr = fmt.Sprintf("%s:%d", wc.Worker.IPAddress, route.RemotePort)
	}
	// Set sticky session cookie so subsequent requests route to same instance.
	http.SetCookie(w, &http.Cookie{
		Name:     "X-Loka-Instance",
		Value:    svc.ID,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   3600,
	})

	p.proxyHTTP(w, r, targetAddr, route, coldStart)
}

// wakeAndWait wakes a service (if idle) or joins an existing wake, then waits
// for the service to become ready. It coalesces concurrent wake requests so
// only one Wake RPC is issued per service. Returns triggered=true only for
// the goroutine that actually initiated the wake call.
func (p *DomainProxy) wakeAndWait(ctx context.Context, serviceID string) (triggered bool, err error) {
	const wakeTimeout = 30 * time.Second

	p.wakeMu.Lock()
	ws, exists := p.wakeInFlight[serviceID]
	if exists {
		// Another goroutine is already waking this service. Wait for it.
		p.wakeMu.Unlock()
		select {
		case <-ws.done:
			return false, ws.err
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wakeTimeout):
			return false, fmt.Errorf("timed out waiting for service wake")
		}
	}

	// We are the first: create wake state and trigger wake.
	ws = &wakeState{done: make(chan struct{})}
	p.wakeInFlight[serviceID] = ws
	p.wakeMu.Unlock()

	// Clean up the in-flight entry when we are done.
	defer func() {
		close(ws.done)
		p.wakeMu.Lock()
		delete(p.wakeInFlight, serviceID)
		p.wakeMu.Unlock()
	}()

	// Issue the wake call. Wake is idempotent: if already waking, it returns
	// the service in waking state; if already running, it returns immediately.
	wakeCtx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()

	svc, err := p.svcMgr.Get(wakeCtx, serviceID)
	if err != nil {
		ws.err = err
		return true, err
	}

	// Only call Wake if the service is actually idle.
	if svc.Status == loka.ServiceStatusIdle {
		if _, err := p.svcMgr.Wake(wakeCtx, serviceID); err != nil {
			ws.err = err
			return true, err
		}
	}

	// Wait for the service to become ready.
	_, err = p.svcMgr.WaitForReady(wakeCtx, serviceID)
	if err != nil {
		ws.err = fmt.Errorf("service did not become ready: %w", err)
		return true, ws.err
	}
	return true, nil
}

func (p *DomainProxy) proxyHTTP(w http.ResponseWriter, r *http.Request, targetAddr string, route *loka.DomainRoute, coldStart bool) {
	// Check for WebSocket upgrade.
	if isWebSocket(r) {
		p.proxyWebSocket(w, r, targetAddr)
		return
	}

	// Limit request body size to prevent memory exhaustion (100 MB).
	const maxRequestBody = 100 << 20
	body := http.MaxBytesReader(w, r.Body, maxRequestBody)

	// Create the proxy request.
	targetURL := fmt.Sprintf("http://%s%s", targetAddr, r.RequestURI)
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, body)
	if err != nil {
		http.Error(w, "Proxy error", http.StatusBadGateway)
		return
	}

	// Copy headers.
	for k, vv := range r.Header {
		for _, v := range vv {
			proxyReq.Header.Add(k, v)
		}
	}
	proxyReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	proxyReq.Header.Set("X-Forwarded-Host", r.Host)
	if route.SessionID != "" {
		proxyReq.Header.Set("X-Loka-Session", route.SessionID)
	}
	if route.ServiceID != "" {
		proxyReq.Header.Set("X-Loka-Service", route.ServiceID)
	}

	resp, err := p.httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Backend unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	// Add cold-start header if the service was woken for this request.
	if coldStart {
		w.Header().Set("X-Loka-Cold-Start", "true")
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *DomainProxy) proxyWebSocket(w http.ResponseWriter, r *http.Request, targetAddr string) {
	// Hijack the client connection first, then dial backend.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "Hijack failed", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Dial the backend. If it fails, write an HTTP error to the raw connection.
	backendConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		clientConn.Close()
		return
	}
	defer backendConn.Close()

	// Set read deadlines on both connections to prevent indefinite blocking.
	const wsIdleTimeout = 30 * time.Minute
	if tc, ok := clientConn.(*net.TCPConn); ok {
		tc.SetReadDeadline(time.Now().Add(wsIdleTimeout))
	}
	if tc, ok := backendConn.(*net.TCPConn); ok {
		tc.SetReadDeadline(time.Now().Add(wsIdleTimeout))
	}

	// Forward the original request to the backend.
	r.Write(backendConn)

	// Bidirectional copy — wait for both directions to finish.
	var copyWg sync.WaitGroup
	copyWg.Add(2)
	go func() {
		defer copyWg.Done()
		io.Copy(backendConn, clientBuf)
	}()
	go func() {
		defer copyWg.Done()
		io.Copy(clientConn, backendConn)
	}()
	copyWg.Wait()
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// ── API endpoints for managing domain routes ────────────

func (s *Server) registerDomainRoutes(r chi.Router, proxy *DomainProxy) {
	r.Post("/api/v1/sessions/{id}/expose", func(w http.ResponseWriter, r *http.Request) {
		sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		var req struct {
			Domain     string `json:"domain"`
			RemotePort int    `json:"remote_port"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Domain == "" {
			writeError(w, http.StatusBadRequest, "domain is required")
			return
		}
		// Auto-append .loka TLD for bare names.
		if !strings.Contains(req.Domain, ".") {
			req.Domain = req.Domain + ".loka"
		}
		if req.RemotePort <= 0 {
			writeError(w, http.StatusBadRequest, "remote_port is required")
			return
		}

		// Verify session exists.
		if _, err := s.sessionManager.Get(r.Context(), sessionID); err != nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}

		// Check domain not taken.
		if existing := proxy.GetRoute(req.Domain); existing != nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("domain %q already in use by session %s", req.Domain, existing.SessionID))
			return
		}

		route := &loka.DomainRoute{
			ID:        req.Domain,
			Domain:    req.Domain,
			SessionID:  sessionID,
			RemotePort: req.RemotePort,
			Type:       loka.DomainRouteSession,
		}
		proxy.AddRoute(route)

		writeJSON(w, http.StatusCreated, map[string]any{
			"domain":     req.Domain,
			"session_id": sessionID,
			"port":       req.RemotePort,
			"url":        fmt.Sprintf("https://%s", req.Domain),
		})
	})

	r.Delete("/api/v1/sessions/{id}/expose/{domain}", func(w http.ResponseWriter, r *http.Request) {
		domain := chi.URLParam(r, "domain")
		if proxy.RemoveRoute(domain) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "domain": domain})
		} else {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no route for %q", domain))
		}
	})

	r.Get("/api/v1/domains", func(w http.ResponseWriter, r *http.Request) {
		routes := proxy.ListRoutes()
		writeJSON(w, http.StatusOK, map[string]any{"routes": routes})
	})
}
