package worker

import (
	"log/slog"
	"os"
	"testing"
)

func testAgent() *Agent {
	return &Agent{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestHandleUpdateRoutes(t *testing.T) {
	a := testAgent()
	a.HandleUpdateRoutes(42, []ServiceRouteEntry{
		{ID: "svc-1", Name: "web", WorkerIP: "10.0.0.1", Port: 8080},
		{ID: "svc-2", Name: "api", WorkerIP: "10.0.0.2", Port: 9090},
	})

	if a.RouteVersion() != 42 {
		t.Errorf("RouteVersion = %d, want 42", a.RouteVersion())
	}

	route, ok := a.LookupRoute("web")
	if !ok {
		t.Fatal("expected to find route 'web'")
	}
	if route.WorkerIP != "10.0.0.1" {
		t.Errorf("WorkerIP = %q, want 10.0.0.1", route.WorkerIP)
	}
	if route.Port != 8080 {
		t.Errorf("Port = %d, want 8080", route.Port)
	}
}

func TestLookupRoute_NotFound(t *testing.T) {
	a := testAgent()
	_, ok := a.LookupRoute("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestHandleUpdateRoutes_OverwritesPrevious(t *testing.T) {
	a := testAgent()
	a.HandleUpdateRoutes(1, []ServiceRouteEntry{
		{ID: "svc-1", Name: "web", WorkerIP: "10.0.0.1", Port: 8080},
	})
	a.HandleUpdateRoutes(2, []ServiceRouteEntry{
		{ID: "svc-1", Name: "web", WorkerIP: "10.0.0.99", Port: 9999},
	})

	route, _ := a.LookupRoute("web")
	if route.WorkerIP != "10.0.0.99" {
		t.Errorf("expected updated WorkerIP, got %s", route.WorkerIP)
	}
	if a.RouteVersion() != 2 {
		t.Errorf("expected version 2, got %d", a.RouteVersion())
	}
}

func TestHandleUpdateRoutes_EmptyServicesList(t *testing.T) {
	a := testAgent()
	a.HandleUpdateRoutes(99, []ServiceRouteEntry{})
	if a.RouteVersion() != 99 {
		t.Errorf("version = %d, want 99", a.RouteVersion())
	}
	_, ok := a.LookupRoute("anything")
	if ok {
		t.Error("expected no routes after empty update")
	}
}

func TestRouteVersion_InitialValue(t *testing.T) {
	a := testAgent()
	if a.RouteVersion() != 0 {
		t.Errorf("initial RouteVersion = %d, want 0", a.RouteVersion())
	}
}

func TestHandleUpdateRoutes_EmptyServiceName(t *testing.T) {
	a := testAgent()
	a.HandleUpdateRoutes(1, []ServiceRouteEntry{{Name: "", WorkerIP: "10.0.0.1", Port: 80}})
	route, ok := a.LookupRoute("")
	if !ok {
		t.Fatal("expected route stored under empty key")
	}
	if route.WorkerIP != "10.0.0.1" {
		t.Errorf("WorkerIP = %q, want 10.0.0.1", route.WorkerIP)
	}
}

func TestSetRemoteMode(t *testing.T) {
	a := testAgent()
	if a.remoteMode {
		t.Error("expected remoteMode=false by default")
	}
	a.SetRemoteMode(true)
	if !a.remoteMode {
		t.Error("expected remoteMode=true after SetRemoteMode(true)")
	}
}
