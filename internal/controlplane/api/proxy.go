package api

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/controlplane/session"
	"github.com/vyprai/loka/internal/controlplane/worker"
	"github.com/vyprai/loka/internal/loka"
)

// DomainProxy is a reverse proxy that routes HTTP requests by subdomain
// to specific ports inside session VMs.
//
// Example: my-app.loka.example.com → session abc123, port 5000
type DomainProxy struct {
	baseDomain string
	sm         *session.Manager
	registry   *worker.Registry
	logger     *slog.Logger

	mu     sync.RWMutex
	routes map[string]*loka.DomainRoute // subdomain → route
}

// NewDomainProxy creates a domain-based reverse proxy.
func NewDomainProxy(baseDomain string, sm *session.Manager, registry *worker.Registry, logger *slog.Logger) *DomainProxy {
	return &DomainProxy{
		baseDomain: baseDomain,
		sm:         sm,
		registry:   registry,
		logger:     logger,
		routes:     make(map[string]*loka.DomainRoute),
	}
}

// AddRoute registers a subdomain → session:port mapping.
func (p *DomainProxy) AddRoute(route *loka.DomainRoute) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[route.Subdomain] = route
	p.logger.Info("domain route added",
		"subdomain", route.Subdomain,
		"session", route.SessionID,
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
		// In production, this would connect to the worker's VM via the tunnel.
		// For now, proxy to the worker's IP on the specified port.
		targetAddr := fmt.Sprintf("%s:%d", wc.Worker.IPAddress, route.RemotePort)
		p.proxyHTTP(w, r, targetAddr, route)
	})
}

func (p *DomainProxy) proxyHTTP(w http.ResponseWriter, r *http.Request, targetAddr string, route *loka.DomainRoute) {
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
	proxyReq.Header.Set("X-Loka-Session", route.SessionID)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(proxyReq)
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
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *DomainProxy) proxyWebSocket(w http.ResponseWriter, r *http.Request, targetAddr string) {
	// Dial the backend.
	backendConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		http.Error(w, "Backend unreachable", http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	// Hijack the client connection.
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

	// Forward the original request to the backend.
	r.Write(backendConn)

	// Bidirectional copy.
	done := make(chan struct{}, 2)
	go func() { io.Copy(backendConn, clientBuf); done <- struct{}{} }()
	go func() { io.Copy(clientConn, backendConn); done <- struct{}{} }()
	<-done
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
