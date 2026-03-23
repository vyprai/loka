package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/vyprai/loka/internal/loka"
)

// newTestProxy creates a DomainProxy with nil manager/registry (only used
// for route storage and handler tests that don't hit the backend).
func newTestProxy(t *testing.T) *DomainProxy {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewDomainProxy("loka.example.com", nil, nil, logger)
}

func TestAddRoute(t *testing.T) {
	p := newTestProxy(t)

	route := &loka.DomainRoute{
		ID:         "my-app",
		Subdomain:  "my-app",
		SessionID:  "sess-123",
		RemotePort: 5000,
	}
	p.AddRoute(route)

	got := p.GetRoute("my-app")
	if got == nil {
		t.Fatal("expected route, got nil")
	}
	if got.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-123")
	}
	if got.RemotePort != 5000 {
		t.Errorf("RemotePort = %d, want 5000", got.RemotePort)
	}
}

func TestRemoveRoute(t *testing.T) {
	p := newTestProxy(t)

	p.AddRoute(&loka.DomainRoute{
		ID:        "remove-me",
		Subdomain: "remove-me",
		SessionID: "sess-456",
	})

	ok := p.RemoveRoute("remove-me")
	if !ok {
		t.Fatal("RemoveRoute returned false, expected true")
	}

	got := p.GetRoute("remove-me")
	if got != nil {
		t.Fatalf("expected nil after removal, got %+v", got)
	}
}

func TestListRoutes(t *testing.T) {
	p := newTestProxy(t)

	// Initially empty.
	routes := p.ListRoutes()
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes, got %d", len(routes))
	}

	// Add two routes.
	p.AddRoute(&loka.DomainRoute{Subdomain: "app-a", SessionID: "s1", RemotePort: 3000})
	p.AddRoute(&loka.DomainRoute{Subdomain: "app-b", SessionID: "s2", RemotePort: 4000})

	routes = p.ListRoutes()
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
}

func TestRemoveNonexistent(t *testing.T) {
	p := newTestProxy(t)

	ok := p.RemoveRoute("does-not-exist")
	if ok {
		t.Fatal("RemoveRoute returned true for non-existent route, expected false")
	}
}

func TestDuplicateSubdomain(t *testing.T) {
	p := newTestProxy(t)

	p.AddRoute(&loka.DomainRoute{Subdomain: "dup", SessionID: "first", RemotePort: 3000})
	p.AddRoute(&loka.DomainRoute{Subdomain: "dup", SessionID: "second", RemotePort: 4000})

	got := p.GetRoute("dup")
	if got == nil {
		t.Fatal("expected route, got nil")
	}
	if got.SessionID != "second" {
		t.Errorf("SessionID = %q, want %q (second add should overwrite)", got.SessionID, "second")
	}
	if got.RemotePort != 4000 {
		t.Errorf("RemotePort = %d, want 4000", got.RemotePort)
	}
}

func TestHandlerUnknownHost(t *testing.T) {
	p := newTestProxy(t)
	handler := p.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "unknown.other-domain.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandlerNoRoute(t *testing.T) {
	p := newTestProxy(t)
	handler := p.Handler()

	// Valid subdomain format but no route registered.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "no-such-app.loka.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestAddRoute_MultipleRoutes(t *testing.T) {
	p := newTestProxy(t)

	// Add routes for different sessions/ports.
	p.AddRoute(&loka.DomainRoute{Subdomain: "api", SessionID: "s1", RemotePort: 8080})
	p.AddRoute(&loka.DomainRoute{Subdomain: "web", SessionID: "s2", RemotePort: 3000})
	p.AddRoute(&loka.DomainRoute{Subdomain: "db-admin", SessionID: "s3", RemotePort: 5432})

	routes := p.ListRoutes()
	if len(routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(routes))
	}

	// Verify each route independently.
	api := p.GetRoute("api")
	if api == nil || api.SessionID != "s1" || api.RemotePort != 8080 {
		t.Errorf("api route mismatch: %+v", api)
	}
	web := p.GetRoute("web")
	if web == nil || web.SessionID != "s2" || web.RemotePort != 3000 {
		t.Errorf("web route mismatch: %+v", web)
	}
	dbAdmin := p.GetRoute("db-admin")
	if dbAdmin == nil || dbAdmin.SessionID != "s3" || dbAdmin.RemotePort != 5432 {
		t.Errorf("db-admin route mismatch: %+v", dbAdmin)
	}
}

func TestRemoveRoute_ThenGetReturnsNil(t *testing.T) {
	p := newTestProxy(t)

	p.AddRoute(&loka.DomainRoute{Subdomain: "temp", SessionID: "s1", RemotePort: 9000})

	// Verify it exists.
	if got := p.GetRoute("temp"); got == nil {
		t.Fatal("expected route to exist before removal")
	}

	// Remove and verify.
	ok := p.RemoveRoute("temp")
	if !ok {
		t.Fatal("RemoveRoute returned false")
	}
	if got := p.GetRoute("temp"); got != nil {
		t.Fatalf("expected nil after removal, got %+v", got)
	}

	// List should be empty.
	if routes := p.ListRoutes(); len(routes) != 0 {
		t.Errorf("expected 0 routes after removal, got %d", len(routes))
	}
}

func TestAddRoute_OverwriteUpdatesPort(t *testing.T) {
	p := newTestProxy(t)

	p.AddRoute(&loka.DomainRoute{Subdomain: "svc", SessionID: "s1", RemotePort: 3000})
	p.AddRoute(&loka.DomainRoute{Subdomain: "svc", SessionID: "s1", RemotePort: 4000})

	got := p.GetRoute("svc")
	if got == nil {
		t.Fatal("expected route, got nil")
	}
	if got.RemotePort != 4000 {
		t.Errorf("RemotePort = %d, want 4000 after overwrite", got.RemotePort)
	}

	// Should still be only 1 route.
	if routes := p.ListRoutes(); len(routes) != 1 {
		t.Errorf("expected 1 route after overwrite, got %d", len(routes))
	}
}

func TestHandlerBaseHost_NoSubdomain(t *testing.T) {
	p := newTestProxy(t)
	handler := p.Handler()

	// Request to the base domain itself (no subdomain).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "loka.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for base domain request, got %d", rec.Code)
	}
}
