// Package gateway provides a standalone HTTP reverse proxy for routing
// domain-based traffic to worker VMs. It is designed to run independently
// of the control plane so that active connections survive CP restarts.
//
// The gateway receives route updates via gRPC streaming from the CP.
// Routes contain pre-resolved worker IPs, so the gateway has no dependency
// on the worker registry or database.
package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RouteEntry is a fully-resolved route. The CP resolves worker IPs before
// sending to the gateway, so no registry lookup is needed at proxy time.
type RouteEntry struct {
	Domain          string
	ServiceID       string
	ServiceName     string
	RemotePort      int
	WorkerIP        string // Resolved: worker private or public IP.
	ForwardPort     int    // Vsock-tunnelled port on worker (0 = use RemotePort directly).
	GuestIP         string // Direct VM guest IP via TAP (empty = use worker IP).
	IsSession       bool
	SessionID       string
}

// ServiceMetrics tracks per-service request metrics for autoscaling.
type ServiceMetrics struct {
	ActiveConnections atomic.Int64
	TotalRequests     atomic.Int64
}

// Gateway is a standalone HTTP reverse proxy that routes domain-based
// traffic to worker VMs using pre-resolved routes.
type Gateway struct {
	logger *slog.Logger

	mu     sync.RWMutex
	routes map[string]*RouteEntry // domain → route

	rrCounter      uint64
	serviceMetrics sync.Map // serviceID → *ServiceMetrics

	httpClient *http.Client
}

// New creates a new gateway.
func New(logger *slog.Logger) *Gateway {
	return &Gateway{
		logger: logger,
		routes: make(map[string]*RouteEntry),
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 60 * time.Second,
		},
	}
}

// SetRoutes replaces the entire route table (used for FULL_SYNC).
func (g *Gateway) SetRoutes(routes []*RouteEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()

	newRoutes := make(map[string]*RouteEntry, len(routes))
	for _, r := range routes {
		newRoutes[r.Domain] = r
	}
	g.routes = newRoutes
	g.logger.Info("routes synced", "count", len(routes))
}

// AddRoute adds or updates a single route.
func (g *Gateway) AddRoute(route *RouteEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.routes[route.Domain] = route
	g.logger.Info("route added", "domain", route.Domain, "service", route.ServiceName)
}

// RemoveRoute removes a route by domain.
func (g *Gateway) RemoveRoute(domain string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.routes, domain)
	g.logger.Info("route removed", "domain", domain)
}

// RouteCount returns the number of active routes.
func (g *Gateway) RouteCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.routes)
}

// GetServiceMetrics returns metrics for a service.
func (g *Gateway) GetServiceMetrics(serviceID string) *ServiceMetrics {
	v, _ := g.serviceMetrics.LoadOrStore(serviceID, &ServiceMetrics{})
	return v.(*ServiceMetrics)
}

// GetActiveConnections returns the active connection count for a service.
func (g *Gateway) GetActiveConnections(serviceID string) int64 {
	return g.GetServiceMetrics(serviceID).ActiveConnections.Load()
}

// AllActiveConnections returns active connections for all services.
func (g *Gateway) AllActiveConnections() map[string]int64 {
	result := make(map[string]int64)
	g.serviceMetrics.Range(func(key, value any) bool {
		m := value.(*ServiceMetrics)
		if n := m.ActiveConnections.Load(); n > 0 {
			result[key.(string)] = n
		}
		return true
	})
	return result
}

// Handler returns the HTTP handler for the gateway.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.serveHTTP)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if idx := strings.IndexByte(host, ':'); idx != -1 {
		host = host[:idx]
	}

	g.mu.RLock()
	route, ok := g.routes[host]
	g.mu.RUnlock()

	if !ok {
		http.Error(w, fmt.Sprintf("No route for host %q", host), http.StatusNotFound)
		return
	}

	// Track metrics.
	if route.ServiceID != "" {
		metrics := g.GetServiceMetrics(route.ServiceID)
		metrics.ActiveConnections.Add(1)
		metrics.TotalRequests.Add(1)
		defer metrics.ActiveConnections.Add(-1)
	}

	// Resolve target address from pre-resolved route.
	targetAddr := g.resolveTarget(route)
	if targetAddr == "" {
		http.Error(w, "No backend available", http.StatusBadGateway)
		return
	}

	// Proxy the request.
	if isWebSocket(r) {
		g.proxyWebSocket(w, r, targetAddr)
	} else {
		g.proxyHTTP(w, r, targetAddr, route)
	}
}

// resolveTarget picks the target address from the pre-resolved route.
func (g *Gateway) resolveTarget(route *RouteEntry) string {
	// Prefer direct VM guest IP (TAP networking).
	if route.GuestIP != "" {
		return fmt.Sprintf("%s:%d", route.GuestIP, route.RemotePort)
	}
	// Fall back to vsock-tunnelled forward port on worker.
	if route.ForwardPort > 0 && route.WorkerIP != "" {
		return fmt.Sprintf("%s:%d", route.WorkerIP, route.ForwardPort)
	}
	// Direct worker IP with service port.
	if route.WorkerIP != "" {
		return fmt.Sprintf("%s:%d", route.WorkerIP, route.RemotePort)
	}
	return ""
}

func (g *Gateway) proxyHTTP(w http.ResponseWriter, r *http.Request, targetAddr string, route *RouteEntry) {
	const maxRequestBody = 100 << 20 // 100 MB
	body := http.MaxBytesReader(w, r.Body, maxRequestBody)

	targetURL := fmt.Sprintf("http://%s%s", targetAddr, r.URL.RequestURI())
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

	resp, err := g.httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Backend unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (g *Gateway) proxyWebSocket(w http.ResponseWriter, r *http.Request, targetAddr string) {
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

	backendConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer backendConn.Close()

	const wsIdleTimeout = 30 * time.Minute
	if tc, ok := clientConn.(*net.TCPConn); ok {
		tc.SetReadDeadline(time.Now().Add(wsIdleTimeout))
	}
	if tc, ok := backendConn.(*net.TCPConn); ok {
		tc.SetReadDeadline(time.Now().Add(wsIdleTimeout))
	}

	r.Write(backendConn)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(backendConn, clientBuf) }()
	go func() { defer wg.Done(); io.Copy(clientConn, backendConn) }()
	wg.Wait()
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
