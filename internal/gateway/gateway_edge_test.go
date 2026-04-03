package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════
// Gateway Proxy Edge Cases
// ═══════════════════════════════════════════════════════

// testBackendRoute creates a route pointing to a test backend's address.
// The test backend addr is "host:port", so we use WorkerIP=host, RemotePort=port.
func testBackendRoute(domain string, backendAddr string) *RouteEntry {
	host, portStr, _ := net.SplitHostPort(backendAddr)
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	return &RouteEntry{
		Domain:     domain,
		ServiceID:  "test-svc",
		WorkerIP:   host,
		RemotePort: port,
	}
}

func TestGateway_ProxyToRealBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "ok")
		w.WriteHeader(200)
		w.Write([]byte("backend-response"))
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("proxy.loka", backend.Listener.Addr().String()))

	req := httptest.NewRequest("GET", "http://proxy.loka/path?q=1", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if body != "backend-response" {
		t.Errorf("expected backend-response, got %q", body)
	}
	if w.Header().Get("X-Test") != "ok" {
		t.Error("response header not forwarded")
	}
}

func TestGateway_BackendReturns5xx(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("service unavailable"))
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("err.loka", backend.Listener.Addr().String()))

	req := httptest.NewRequest("GET", "http://err.loka/", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	// Gateway should forward 503 from backend, not generate its own error.
	if w.Code != 503 {
		t.Errorf("expected 503 forwarded from backend, got %d", w.Code)
	}
}

func TestGateway_BackendSlowResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte("slow"))
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("slow.loka", backend.Listener.Addr().String()))

	req := httptest.NewRequest("GET", "http://slow.loka/", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 for slow backend, got %d", w.Code)
	}
}

func TestGateway_LargeResponseBody(t *testing.T) {
	// 1MB response.
	largeBody := strings.Repeat("x", 1024*1024)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(largeBody))
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("large.loka", backend.Listener.Addr().String()))

	req := httptest.NewRequest("GET", "http://large.loka/", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Body.Len() != len(largeBody) {
		t.Errorf("expected %d bytes, got %d", len(largeBody), w.Body.Len())
	}
}

func TestGateway_PostBodyForwarded(t *testing.T) {
	var receivedBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		receivedBody = string(data)
		w.WriteHeader(200)
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("post.loka", backend.Listener.Addr().String()))

	req := httptest.NewRequest("POST", "http://post.loka/upload", strings.NewReader("request-body-data"))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if receivedBody != "request-body-data" {
		t.Errorf("expected body forwarded, got %q", receivedBody)
	}
}

func TestGateway_XForwardedHeaders(t *testing.T) {
	var headers http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header
		w.WriteHeader(200)
	}))
	defer backend.Close()

	gw := New(slog.Default())
	r := testBackendRoute("fwd.loka", backend.Listener.Addr().String())
	r.ServiceID = "svc-123"
	r.SessionID = "sess-456"
	r.IsSession = true
	gw.AddRoute(r)

	req := httptest.NewRequest("GET", "http://fwd.loka/", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if headers.Get("X-Forwarded-Host") != "fwd.loka" {
		t.Errorf("X-Forwarded-Host = %q", headers.Get("X-Forwarded-Host"))
	}
	if headers.Get("X-Loka-Service") != "svc-123" {
		t.Errorf("X-Loka-Service = %q", headers.Get("X-Loka-Service"))
	}
	if headers.Get("X-Loka-Session") != "sess-456" {
		t.Errorf("X-Loka-Session = %q", headers.Get("X-Loka-Session"))
	}
}

func TestGateway_EmptyTargetAddr_Returns502(t *testing.T) {
	gw := New(slog.Default())
	gw.AddRoute(&RouteEntry{
		Domain: "empty.loka",
		// All IPs empty → resolveTarget returns ""
	})

	req := httptest.NewRequest("GET", "http://empty.loka/", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestGateway_HostWithPort(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("porthost.loka", backend.Listener.Addr().String()))

	// Host header includes port.
	req := httptest.NewRequest("GET", "http://porthost.loka:6843/", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 with host:port, got %d", w.Code)
	}
}

// ═══════════════════════════════════════════════════════
// Concurrent Operations
// ═══════════════════════════════════════════════════════

func TestGateway_RouteChangesDuringRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // Simulate slow backend.
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	gw := New(slog.Default())
	gw.AddRoute(func() *RouteEntry { r := testBackendRoute("live.loka", backend.Listener.Addr().String()); r.ServiceID = "svc-1"; return r }())

	var wg sync.WaitGroup
	var requestsDone atomic.Int32

	// Send requests in parallel.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "http://live.loka/", nil)
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, req)
			requestsDone.Add(1)
		}()
	}

	// Mid-flight: replace all routes.
	time.Sleep(10 * time.Millisecond)
	gw.SetRoutes([]*RouteEntry{
		func() *RouteEntry { r := testBackendRoute("live.loka", backend.Listener.Addr().String()); r.ServiceID = "svc-2"; return r }(),
	})

	wg.Wait()
	if requestsDone.Load() != 10 {
		t.Errorf("expected all 10 requests to complete, got %d", requestsDone.Load())
	}
}

func TestGateway_RouteRemovedDuringRequest(t *testing.T) {
	gw := New(slog.Default())
	gw.AddRoute(&RouteEntry{Domain: "ephemeral.loka", WorkerIP: "127.0.0.1", RemotePort: 1})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "http://ephemeral.loka/", nil)
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, req) // May get 502 or 404 — either is fine.
		}()
	}

	// Remove route while requests are in flight.
	gw.RemoveRoute("ephemeral.loka")
	wg.Wait()
	// No panic = success.
}

func TestGateway_ConcurrentMetricsAccess(t *testing.T) {
	gw := New(slog.Default())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			m := gw.GetServiceMetrics(fmt.Sprintf("svc-%d", n%5))
			m.ActiveConnections.Add(1)
			m.TotalRequests.Add(1)
		}(i)
		go func() {
			defer wg.Done()
			gw.AllActiveConnections() // Concurrent read.
		}()
	}
	wg.Wait()
}

// ═══════════════════════════════════════════════════════
// Watcher Edge Cases
// ═══════════════════════════════════════════════════════

func TestWatcher_CP_Returns5xx(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer cp.Close()

	gw := New(slog.Default())
	watcher := NewRouteWatcher(cp.URL, gw, slog.Default())

	err := watcher.syncRoutes(context.Background())
	if err == nil {
		t.Fatal("expected error on 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 in error, got %v", err)
	}
}

func TestWatcher_CP_ReturnsMalformedJSON(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not valid json"))
	}))
	defer cp.Close()

	gw := New(slog.Default())
	watcher := NewRouteWatcher(cp.URL, gw, slog.Default())

	err := watcher.syncRoutes(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestWatcher_CP_ReturnsEmptyArray(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[]"))
	}))
	defer cp.Close()

	gw := New(slog.Default())
	gw.AddRoute(&RouteEntry{Domain: "old.loka"}) // Pre-existing route.

	watcher := NewRouteWatcher(cp.URL, gw, slog.Default())
	err := watcher.syncRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Routes should be replaced (empty = all routes removed).
	if gw.RouteCount() != 0 {
		t.Errorf("expected 0 routes after empty sync, got %d", gw.RouteCount())
	}
}

func TestWatcher_CP_ReturnsNullBody(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("null"))
	}))
	defer cp.Close()

	gw := New(slog.Default())
	watcher := NewRouteWatcher(cp.URL, gw, slog.Default())

	err := watcher.syncRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// null decodes to nil slice → SetRoutes with nil → empty map.
	if gw.RouteCount() != 0 {
		t.Errorf("expected 0 routes after null sync, got %d", gw.RouteCount())
	}
}

func TestWatcher_ContextCancelled(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // Very slow.
	}))
	defer cp.Close()

	gw := New(slog.Default())
	watcher := NewRouteWatcher(cp.URL, gw, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := watcher.syncRoutes(ctx)
	if err == nil {
		t.Fatal("expected error on context timeout")
	}
}

func TestWatcher_PreservesRoutesOnFailure(t *testing.T) {
	gw := New(slog.Default())
	gw.SetRoutes([]*RouteEntry{
		{Domain: "cached.loka", ServiceID: "svc-cached"},
		{Domain: "other.loka", ServiceID: "svc-other"},
	})

	watcher := NewRouteWatcher("http://127.0.0.1:1", gw, slog.Default())
	watcher.syncRoutes(context.Background()) // Fails — CP unreachable.

	// Old routes must be preserved.
	if gw.RouteCount() != 2 {
		t.Errorf("expected 2 cached routes preserved, got %d", gw.RouteCount())
	}
}

func TestWatcher_StartStopsOnContextCancel(t *testing.T) {
	gw := New(slog.Default())
	watcher := NewRouteWatcher("http://127.0.0.1:1", gw, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		watcher.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Good — Start returned after context cancelled.
	case <-time.After(3 * time.Second):
		t.Fatal("Start didn't return after context cancel")
	}
}

// ═══════════════════════════════════════════════════════
// Metrics Reporter Edge Cases
// ═══════════════════════════════════════════════════════

func TestMetricsReporter_CPDown_SilentDrop(t *testing.T) {
	gw := New(slog.Default())
	gw.GetServiceMetrics("svc-1").ActiveConnections.Store(5)

	watcher := NewRouteWatcher("http://127.0.0.1:1", gw, slog.Default())
	// Should not panic or hang.
	watcher.reportMetrics(context.Background())
}

func TestMetricsReporter_NoMetrics_Skips(t *testing.T) {
	gw := New(slog.Default())
	watcher := NewRouteWatcher("http://localhost:1", gw, slog.Default())
	// No active connections → should return immediately.
	watcher.reportMetrics(context.Background())
}

func TestMetricsReporter_CPReceivesMetrics(t *testing.T) {
	var received map[string]int64
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(200)
	}))
	defer cp.Close()

	gw := New(slog.Default())
	gw.GetServiceMetrics("svc-a").ActiveConnections.Store(3)
	gw.GetServiceMetrics("svc-b").ActiveConnections.Store(7)

	watcher := NewRouteWatcher(cp.URL, gw, slog.Default())
	watcher.reportMetrics(context.Background())

	if received["svc-a"] != 3 {
		t.Errorf("expected svc-a=3, got %d", received["svc-a"])
	}
	if received["svc-b"] != 7 {
		t.Errorf("expected svc-b=7, got %d", received["svc-b"])
	}
}

func TestMetricsReporter_StartsAndStops(t *testing.T) {
	gw := New(slog.Default())
	watcher := NewRouteWatcher("http://127.0.0.1:1", gw, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		watcher.StartMetricsReporter(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("metrics reporter didn't stop after context cancel")
	}
}

// ═══════════════════════════════════════════════════════
// WebSocket Proxy
// ═══════════════════════════════════════════════════════

func TestGateway_WebSocket_BackendDown(t *testing.T) {
	gw := New(slog.Default())
	gw.AddRoute(&RouteEntry{
		Domain:     "ws.loka",
		WorkerIP:   "127.0.0.1",
		RemotePort: 1, // Nothing listening.
	})

	// Simulate WebSocket upgrade request.
	req := httptest.NewRequest("GET", "http://ws.loka/", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	// WebSocket requires hijack which httptest.ResponseRecorder doesn't support.
	// Should get 500 (Hijack not supported) or 502 (dial failed).
	if w.Code != http.StatusInternalServerError && w.Code != http.StatusBadGateway {
		t.Errorf("expected 500 or 502 for WS without hijack, got %d", w.Code)
	}
}

func TestGateway_WebSocket_RealTCPProxy(t *testing.T) {
	// Backend echo server.
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendListener.Close()

	go func() {
		for {
			conn, err := backendListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read request then echo back.
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n"))
				c.Write(buf[:n]) // Echo.
			}(conn)
		}
	}()

	gw := New(slog.Default())
	gw.AddRoute(testBackendRoute("wsreal.loka", backendListener.Addr().String()))

	// Create a real TCP server that proxies through the gateway.
	gwListener, _ := net.Listen("tcp", "127.0.0.1:0")
	defer gwListener.Close()

	gwSrv := &http.Server{Handler: gw.Handler()}
	go gwSrv.Serve(gwListener)
	defer gwSrv.Close()

	// Connect to the gateway as a client.
	conn, err := net.DialTimeout("tcp", gwListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	// Send WebSocket upgrade request.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: wsreal.loka\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, "101") {
		t.Errorf("expected 101 Switching Protocols, got: %s", resp[:min(len(resp), 80)])
	}
}

// ═══════════════════════════════════════════════════════
// Route Resolution Priority
// ═══════════════════════════════════════════════════════

func TestGateway_ResolveTarget_Priority(t *testing.T) {
	gw := New(slog.Default())

	// GuestIP takes priority over everything.
	r1 := &RouteEntry{GuestIP: "10.0.0.1", WorkerIP: "10.0.0.2", ForwardPort: 5000, RemotePort: 8080}
	if got := gw.resolveTarget(r1); got != "10.0.0.1:8080" {
		t.Errorf("GuestIP should take priority, got %q", got)
	}

	// ForwardPort takes priority over direct RemotePort.
	r2 := &RouteEntry{WorkerIP: "10.0.0.2", ForwardPort: 5000, RemotePort: 8080}
	if got := gw.resolveTarget(r2); got != "10.0.0.2:5000" {
		t.Errorf("ForwardPort should take priority, got %q", got)
	}

	// Direct worker IP + RemotePort.
	r3 := &RouteEntry{WorkerIP: "10.0.0.2", RemotePort: 8080}
	if got := gw.resolveTarget(r3); got != "10.0.0.2:8080" {
		t.Errorf("expected direct worker, got %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
