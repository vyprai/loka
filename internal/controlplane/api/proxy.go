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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
)

// DomainProxy is a reverse proxy that routes HTTP requests by subdomain
// to specific ports inside session VMs or deployed services.
//
// For service routes, the proxy supports cold-start wake-on-request:
// if a service is idle, the first request triggers a wake and blocks
// until the service is ready (up to 30s).
//
// Example: my-app.loka.example.com -> session abc123, port 5000
type DomainProxy struct {
	baseDomain string
	sm         *session.Manager
	svcMgr     *service.Manager // Service manager for cold-start wake.
	registry   *worker.Registry
	logger     *slog.Logger

	mu     sync.RWMutex
	routes map[string]*loka.DomainRoute // subdomain -> route

	// wakeMu protects wakeInFlight to coalesce concurrent wake requests.
	wakeMu       sync.Mutex
	wakeInFlight map[string]*wakeState // serviceID -> in-progress wake

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
func NewDomainProxy(baseDomain string, sm *session.Manager, registry *worker.Registry, logger *slog.Logger, opts ...DomainProxyOpts) *DomainProxy {
	var o DomainProxyOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	return &DomainProxy{
		baseDomain:   baseDomain,
		sm:           sm,
		svcMgr:       o.ServiceManager,
		registry:     registry,
		logger:       logger,
		routes:       make(map[string]*loka.DomainRoute),
		wakeInFlight: make(map[string]*wakeState),
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

// DomainProxyOpts holds optional configuration for the domain proxy.
type DomainProxyOpts struct {
	ServiceManager *service.Manager
}

// AddRoute registers a subdomain -> session/service:port mapping.
func (p *DomainProxy) AddRoute(route *loka.DomainRoute) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[route.Subdomain] = route

	routeType := string(route.Type)
	if routeType == "" {
		routeType = "session"
	}
	targetID := route.SessionID
	if route.Type == loka.DomainRouteService {
		targetID = route.ServiceID
	}
	p.logger.Info("domain route added",
		"subdomain", route.Subdomain,
		"type", routeType,
		"target", targetID,
		"port", route.RemotePort,
		"url", fmt.Sprintf("https://%s.%s", route.Subdomain, p.baseDomain))
}

// RemoveRoute removes a subdomain mapping.
func (p *DomainProxy) RemoveRoute(subdomain string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.routes[subdomain]
	delete(p.routes, subdomain)
	return ok
}

// GetRoute returns a route by subdomain.
func (p *DomainProxy) GetRoute(subdomain string) *loka.DomainRoute {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.routes[subdomain]
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

		// Extract subdomain from host.
		subdomain := ""
		if strings.HasSuffix(host, "."+p.baseDomain) {
			subdomain = strings.TrimSuffix(host, "."+p.baseDomain)
		}

		if subdomain == "" {
			http.Error(w, fmt.Sprintf("Unknown host: %s. Expected *.%s", host, p.baseDomain), http.StatusNotFound)
			return
		}

		route := p.GetRoute(subdomain)
		if route == nil {
			http.Error(w, fmt.Sprintf("No route for %s.%s", subdomain, p.baseDomain), http.StatusNotFound)
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
	targetAddr := fmt.Sprintf("%s:%d", wc.Worker.IPAddress, route.RemotePort)
	p.proxyHTTP(w, r, targetAddr, route, false)
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

	// Proxy the request.
	targetAddr := fmt.Sprintf("%s:%d", wc.Worker.IPAddress, route.RemotePort)
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

	// Create the proxy request.
	targetURL := fmt.Sprintf("http://%s%s", targetAddr, r.RequestURI)
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
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
		sessionID := chi.URLParam(r, "id")
		var req struct {
			Subdomain  string `json:"subdomain"`
			RemotePort int    `json:"remote_port"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Subdomain == "" {
			writeError(w, http.StatusBadRequest, "subdomain is required")
			return
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

		// Check subdomain not taken.
		if existing := proxy.GetRoute(req.Subdomain); existing != nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("subdomain %q already in use by session %s", req.Subdomain, existing.SessionID))
			return
		}

		route := &loka.DomainRoute{
			ID:         req.Subdomain,
			Subdomain:  req.Subdomain,
			SessionID:  sessionID,
			RemotePort: req.RemotePort,
			Type:       loka.DomainRouteSession,
		}
		proxy.AddRoute(route)

		writeJSON(w, http.StatusCreated, map[string]any{
			"subdomain":  req.Subdomain,
			"session_id": sessionID,
			"port":       req.RemotePort,
			"url":        fmt.Sprintf("https://%s.%s", req.Subdomain, proxy.baseDomain),
		})
	})

	r.Delete("/api/v1/sessions/{id}/expose/{subdomain}", func(w http.ResponseWriter, r *http.Request) {
		subdomain := chi.URLParam(r, "subdomain")
		if proxy.RemoveRoute(subdomain) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "subdomain": subdomain})
		} else {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no route for %q", subdomain))
		}
	})

	r.Get("/api/v1/domains", func(w http.ResponseWriter, r *http.Request) {
		routes := proxy.ListRoutes()
		writeJSON(w, http.StatusOK, map[string]any{"routes": routes, "base_domain": proxy.baseDomain})
	})
}
