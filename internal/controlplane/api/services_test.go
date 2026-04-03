package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vyprai/loka/internal/controlplane/service"
	"github.com/vyprai/loka/internal/loka"
)

// setupServiceTestServer creates a test server backed by an in-memory SQLite
// store with a real service.Manager. A test worker is registered so deploys
// can be scheduled.
func setupServiceTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := setupTestServer(t)
	ts.registerTestWorker(t)

	svcMgr := service.NewManager(ts.store, ts.registry, ts.sched, ts.imgMgr, nil, nil, ts.server.logger, nil)
	t.Cleanup(func() { svcMgr.Close() })
	ts.server.serviceManager = svcMgr
	return ts
}

// createTestService inserts a service directly into the store, bypassing
// the async deploy flow, so tests get a deterministic record.
func createTestService(t *testing.T, ts *testServer, name string, status loka.ServiceStatus) *loka.Service {
	t.Helper()
	now := time.Now()
	svc := &loka.Service{
		ID:        "svc-" + name,
		Name:      name,
		Status:    status,
		ImageRef:  "alpine:latest",
		Port:      8080,
		VCPUs:     1,
		MemoryMB:  512,
		Env:       map[string]string{"FOO": "bar"},
		Labels:    map[string]string{},
		Routes:    []loka.ServiceRoute{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := ts.store.Services().Create(context.Background(), svc); err != nil {
		t.Fatalf("create test service: %v", err)
	}
	return svc
}

// --- Tests ---

func TestDeployService(t *testing.T) {
	ts := setupServiceTestServer(t)

	payload := map[string]any{
		"name":      "web-app",
		"image":     "alpine:latest",
		"port":      8080,
		"vcpus":     1,
		"memory_mb": 512,
	}
	rec := ts.doRequest(t, http.MethodPost, "/api/v1/services", payload, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var svc loka.Service
	decodeBody(t, rec, &svc)
	if svc.Name != "web-app" {
		t.Errorf("expected name web-app, got %q", svc.Name)
	}
	if svc.Port != 8080 {
		t.Errorf("expected port 8080, got %d", svc.Port)
	}
	if svc.ID == "" {
		t.Error("expected non-empty service ID")
	}
	if svc.Status != loka.ServiceStatusDeploying {
		t.Errorf("expected status deploying, got %q", svc.Status)
	}
}

func TestDeployServiceInvalid(t *testing.T) {
	ts := setupServiceTestServer(t)

	// Send an invalid JSON body.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/services", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListServices(t *testing.T) {
	ts := setupServiceTestServer(t)
	createTestService(t, ts, "svc-a", loka.ServiceStatusRunning)
	createTestService(t, ts, "svc-b", loka.ServiceStatusStopped)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Services []loka.Service `json:"services"`
		Total    int            `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total < 2 {
		t.Errorf("expected at least 2 services, got %d", body.Total)
	}
}

func TestListServicesFilter(t *testing.T) {
	ts := setupServiceTestServer(t)
	createTestService(t, ts, "running-svc", loka.ServiceStatusRunning)
	createTestService(t, ts, "stopped-svc", loka.ServiceStatusStopped)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services?status=running", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Services []loka.Service `json:"services"`
		Total    int            `json:"total"`
	}
	decodeBody(t, rec, &body)
	for _, s := range body.Services {
		if s.Status != loka.ServiceStatusRunning {
			t.Errorf("expected all services to be running, got %q for %s", s.Status, s.Name)
		}
	}
}

func TestGetService(t *testing.T) {
	ts := setupServiceTestServer(t)
	created := createTestService(t, ts, "get-me", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services/"+created.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var svc loka.Service
	decodeBody(t, rec, &svc)
	if svc.ID != created.ID {
		t.Errorf("expected ID %s, got %s", created.ID, svc.ID)
	}
	if svc.Name != "get-me" {
		t.Errorf("expected name get-me, got %q", svc.Name)
	}
}

func TestGetServiceNotFound(t *testing.T) {
	ts := setupServiceTestServer(t)

	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services/nonexistent-id", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDestroyService(t *testing.T) {
	ts := setupServiceTestServer(t)
	created := createTestService(t, ts, "destroy-me", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodDelete, "/api/v1/services/"+created.ID, nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify it is gone.
	_, err := ts.store.Services().Get(context.Background(), created.ID)
	if err == nil {
		t.Error("expected error fetching destroyed service, got nil")
	}
}

func TestStopService(t *testing.T) {
	ts := setupServiceTestServer(t)
	created := createTestService(t, ts, "stop-me", loka.ServiceStatusRunning)

	rec := ts.doRequest(t, http.MethodPost, "/api/v1/services/"+created.ID+"/stop", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var svc loka.Service
	decodeBody(t, rec, &svc)
	if svc.Status != loka.ServiceStatusStopped {
		t.Errorf("expected status stopped, got %q", svc.Status)
	}
}

func TestUpdateServiceEnv(t *testing.T) {
	ts := setupServiceTestServer(t)
	// Use stopped status so UpdateEnv does not trigger a redeploy.
	created := createTestService(t, ts, "env-svc", loka.ServiceStatusStopped)

	payload := map[string]any{
		"env": map[string]string{"NEW_VAR": "new_value"},
	}
	rec := ts.doRequest(t, http.MethodPut, "/api/v1/services/"+created.ID+"/env", payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var svc loka.Service
	decodeBody(t, rec, &svc)
	if svc.Env["NEW_VAR"] != "new_value" {
		t.Errorf("expected env NEW_VAR=new_value, got %v", svc.Env)
	}
}

func TestGetServiceLogs(t *testing.T) {
	ts := setupServiceTestServer(t)

	// Logs on a stopped service should fail.
	created := createTestService(t, ts, "logs-svc", loka.ServiceStatusStopped)
	rec := ts.doRequest(t, http.MethodGet, "/api/v1/services/"+created.ID+"/logs", nil, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for stopped service logs, got %d: %s", rec.Code, rec.Body.String())
	}

	// Logs on a running service without logsFn should also fail.
	running := createTestService(t, ts, "logs-running", loka.ServiceStatusRunning)
	rec2 := ts.doRequest(t, http.MethodGet, "/api/v1/services/"+running.ID+"/logs?lines=10", nil, nil)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 without logsFn, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestServiceRoutes(t *testing.T) {
	ts := setupServiceTestServer(t)
	created := createTestService(t, ts, "route-svc", loka.ServiceStatusRunning)
	svcURL := "/api/v1/services/" + created.ID

	// POST: add a route.
	addPayload := map[string]any{
		"domain": "myapp",
		"port":   8080,
	}
	rec := ts.doRequest(t, http.MethodPost, svcURL+"/routes", addPayload, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add route: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var svcWithRoute loka.Service
	decodeBody(t, rec, &svcWithRoute)
	if len(svcWithRoute.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(svcWithRoute.Routes))
	}
	// Bare name "myapp" should be auto-expanded to "myapp.loka".
	if svcWithRoute.Routes[0].Domain != "myapp.loka" {
		t.Errorf("expected domain myapp.loka, got %q", svcWithRoute.Routes[0].Domain)
	}

	// GET: list routes.
	rec2 := ts.doRequest(t, http.MethodGet, svcURL+"/routes", nil, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("list routes: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var routeBody struct {
		Routes []loka.ServiceRoute `json:"routes"`
	}
	decodeBody(t, rec2, &routeBody)
	if len(routeBody.Routes) != 1 {
		t.Fatalf("expected 1 route in list, got %d", len(routeBody.Routes))
	}

	// DELETE: remove the route (use expanded domain name).
	rec3 := ts.doRequest(t, http.MethodDelete, svcURL+"/routes/myapp.loka", nil, nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("remove route: expected 200, got %d: %s", rec3.Code, rec3.Body.String())
	}

	// Verify route is gone.
	rec4 := ts.doRequest(t, http.MethodGet, svcURL+"/routes", nil, nil)
	var routeBody2 struct {
		Routes []loka.ServiceRoute `json:"routes"`
	}
	decodeBody(t, rec4, &routeBody2)
	if len(routeBody2.Routes) != 0 {
		t.Errorf("expected 0 routes after delete, got %d", len(routeBody2.Routes))
	}
}
